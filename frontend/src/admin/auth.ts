// Password persistence, key-compatible with the previous panel so "记住密码" survives
// the upgrade: sessionStorage always, localStorage only when remembered.
const SS = () => window.sessionStorage
const LS = () => window.localStorage

export function initialPassword(): string {
  try {
    if (LS().getItem('kiro_remember') !== '1') {
      LS().removeItem('admin_password')
      LS().removeItem('admin_login_time')
    }
    return SS().getItem('admin_password') || LS().getItem('admin_password') || ''
  } catch {
    return ''
  }
}

export function rememberedPassword(): string {
  try {
    return LS().getItem('kiro_remember') === '1' ? LS().getItem('kiro_remembered_pwd') || '' : ''
  } catch {
    return ''
  }
}

export function isRemembered(): boolean {
  try {
    return LS().getItem('kiro_remember') === '1'
  } catch {
    return false
  }
}

export function storePassword(p: string, remember: boolean) {
  const now = Date.now().toString()
  try {
    SS().setItem('admin_password', p)
    SS().setItem('admin_login_time', now)
    if (remember) {
      LS().setItem('admin_password', p)
      LS().setItem('admin_login_time', now)
      LS().setItem('kiro_remember', '1')
      LS().setItem('kiro_remembered_pwd', p)
    } else {
      LS().removeItem('admin_password')
      LS().removeItem('admin_login_time')
      LS().removeItem('kiro_remember')
      LS().removeItem('kiro_remembered_pwd')
    }
  } catch {
    /* storage unavailable */
  }
}

/** Sign out: drop the active session but keep the "remembered" prefill if the operator opted in. */
export function clearPassword() {
  try {
    SS().removeItem('admin_password')
    SS().removeItem('admin_login_time')
    LS().removeItem('admin_password')
    LS().removeItem('admin_login_time')
  } catch {
    /* ignore */
  }
}
