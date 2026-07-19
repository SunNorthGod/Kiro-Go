package config

import (
	"path/filepath"
	"testing"
)

// Shared-pool ("额度池") integrity tests: children commit max(grant, spend) while
// alive, deleting a consumed child settles its spend into the parent, and every
// pool check runs atomically with its mutation.

func newPoolFixture(t *testing.T) (parentID string) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	parent, err := AddApiKey(ApiKeyEntry{Name: "parent", Key: "sk-parent", Enabled: true, CreditsGranted: 100})
	if err != nil {
		t.Fatalf("add parent: %v", err)
	}
	return parent.ID
}

func TestCreateChildApiKeyAtomicPoolCheck(t *testing.T) {
	parentID := newPoolFixture(t)

	child, err := CreateChildApiKey(ApiKeyEntry{
		Name: "child-a", Key: "sk-child-a", Enabled: true, ParentKeyID: parentID,
	}, 60, "reseller:parent", "opening")
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if child.CreditsGranted != 60 {
		t.Fatalf("child grant = %v, want 60", child.CreditsGranted)
	}
	if free := AllocatableChildCredits(parentID, ""); free != 40 {
		t.Fatalf("allocatable after 60 grant = %v, want 40", free)
	}
	// Second child exceeding the remaining pool must be rejected.
	if _, err := CreateChildApiKey(ApiKeyEntry{
		Name: "child-b", Key: "sk-child-b", Enabled: true, ParentKeyID: parentID,
	}, 41, "reseller:parent", "opening"); err == nil {
		t.Fatalf("expected over-pool child creation to fail")
	}
	// Exactly the remainder is fine.
	if _, err := CreateChildApiKey(ApiKeyEntry{
		Name: "child-c", Key: "sk-child-c", Enabled: true, ParentKeyID: parentID,
	}, 40, "reseller:parent", "opening"); err != nil {
		t.Fatalf("exact-fit child creation failed: %v", err)
	}
}

// Deleting a consumed child must NOT refund its consumed credits to the pool:
// the spend is settled into the parent's CreditsUsed, only the unused remainder
// of the grant flows back.
func TestDeleteChildSettlesConsumptionIntoParent(t *testing.T) {
	parentID := newPoolFixture(t)

	child, err := CreateChildApiKey(ApiKeyEntry{
		Name: "child", Key: "sk-child", Enabled: true, ParentKeyID: parentID,
	}, 50, "reseller:parent", "opening")
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// Child consumes 30 of its 50 grant.
	if err := RecordApiKeyUsage(child.ID, "m", 10, 10, 30); err != nil {
		t.Fatalf("record child usage: %v", err)
	}
	if free := AllocatableChildCredits(parentID, ""); free != 50 {
		t.Fatalf("allocatable with live child = %v, want 50", free)
	}

	if err := DeleteApiKey(child.ID); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	parent := GetApiKeyEntry(parentID)
	if parent == nil {
		t.Fatalf("parent missing")
	}
	if parent.CreditsUsed != 30 {
		t.Fatalf("parent.CreditsUsed after settlement = %v, want 30", parent.CreditsUsed)
	}
	// Pool recovers only the unused 20 of the child's grant: 100 - 30 = 70.
	if free := AllocatableChildCredits(parentID, ""); free != 70 {
		t.Fatalf("allocatable after delete = %v, want 70", free)
	}

	// The old exploit: repeat create/consume/delete — pool must keep shrinking.
	child2, err := CreateChildApiKey(ApiKeyEntry{
		Name: "child2", Key: "sk-child2", Enabled: true, ParentKeyID: parentID,
	}, 70, "reseller:parent", "opening")
	if err != nil {
		t.Fatalf("create child2: %v", err)
	}
	if err := RecordApiKeyUsage(child2.ID, "m", 10, 10, 70); err != nil {
		t.Fatalf("record child2 usage: %v", err)
	}
	if err := DeleteApiKey(child2.ID); err != nil {
		t.Fatalf("delete child2: %v", err)
	}
	if free := AllocatableChildCredits(parentID, ""); free != 0 {
		t.Fatalf("allocatable after consuming everything = %v, want 0", free)
	}
}

// An overdrafted child (spend > grant) commits its full spend to the pool.
func TestOverdraftedChildCommitsFullSpend(t *testing.T) {
	parentID := newPoolFixture(t)

	child, err := CreateChildApiKey(ApiKeyEntry{
		Name: "child", Key: "sk-child", Enabled: true, ParentKeyID: parentID,
	}, 20, "reseller:parent", "opening")
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// Concurrent overdraft pushed the child past its grant: used 35 > granted 20.
	if err := RecordApiKeyUsage(child.ID, "m", 1, 1, 35); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Live child commits max(20, 35) = 35 → allocatable 100 - 35 = 65.
	if free := AllocatableChildCredits(parentID, ""); free != 65 {
		t.Fatalf("allocatable with overdrafted child = %v, want 65", free)
	}
	if err := DeleteApiKey(child.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Settlement folds the full 35 into the parent.
	if free := AllocatableChildCredits(parentID, ""); free != 65 {
		t.Fatalf("allocatable after settling overdraft = %v, want 65", free)
	}
}

func TestSetApiKeyGrantCheckedEnforcesInvariants(t *testing.T) {
	parentID := newPoolFixture(t)

	child, err := CreateChildApiKey(ApiKeyEntry{
		Name: "child", Key: "sk-child", Enabled: true, ParentKeyID: parentID,
	}, 50, "reseller:parent", "opening")
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := RecordApiKeyUsage(child.ID, "m", 1, 1, 30); err != nil {
		t.Fatalf("record: %v", err)
	}

	// Child cannot shrink below its own spend.
	if err := SetApiKeyGrantChecked(child.ID, 29); err == nil {
		t.Fatalf("expected shrink below spend to fail")
	}
	// Child cannot grow past the parent pool (100 - 0 own used = 100 max).
	if err := SetApiKeyGrantChecked(child.ID, 101); err == nil {
		t.Fatalf("expected grow past pool to fail")
	}
	// Valid resize inside [spend, pool].
	if err := SetApiKeyGrantChecked(child.ID, 30); err != nil {
		t.Fatalf("valid resize failed: %v", err)
	}

	// Parent cannot shrink below what children commit (30) + own used (0).
	if err := SetApiKeyGrantChecked(parentID, 29); err == nil {
		t.Fatalf("expected parent shrink below commitments to fail")
	}
	if err := SetApiKeyGrantChecked(parentID, 30); err != nil {
		t.Fatalf("parent shrink to exactly committed failed: %v", err)
	}
}

func TestRechargeChildChecksParentPool(t *testing.T) {
	parentID := newPoolFixture(t)

	child, err := CreateChildApiKey(ApiKeyEntry{
		Name: "child", Key: "sk-child", Enabled: true, ParentKeyID: parentID,
	}, 60, "reseller:parent", "opening")
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// Top-up beyond the pool remainder (40) must fail; within it must pass.
	if err := RechargeApiKey(child.ID, 41, "admin", ""); err == nil {
		t.Fatalf("expected over-pool child recharge to fail")
	}
	if err := RechargeApiKey(child.ID, 40, "admin", ""); err != nil {
		t.Fatalf("in-pool child recharge failed: %v", err)
	}
	// Standalone (parent) recharge is unaffected by the pool logic.
	if err := RechargeApiKey(parentID, 5, "admin", ""); err != nil {
		t.Fatalf("parent recharge failed: %v", err)
	}
}
