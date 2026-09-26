import { type ReactNode, useId } from 'react'
import clsx from 'clsx'
import { Checkbox as CheckboxPrimitive, Select as SelectPrimitive, Switch as SwitchPrimitive, Tooltip as TooltipPrimitive } from 'radix-ui'
import { Check, ChevronDown, Minus } from 'lucide-react'
import { motion } from 'motion/react'

/* ---------------- Switch ---------------- */
export function Switch({
  checked,
  onChange,
  disabled,
  accent,
  label,
  id,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  disabled?: boolean
  accent?: boolean
  label?: string
  id?: string
}) {
  return (
    <SwitchPrimitive.Root
      id={id}
      className="switch"
      checked={checked}
      onCheckedChange={onChange}
      disabled={disabled}
      data-accent={accent ? '' : undefined}
      aria-label={label}
    >
      <SwitchPrimitive.Thumb className="switch-thumb" />
    </SwitchPrimitive.Root>
  )
}

/** A switch with a title and an explanation next to it; the whole row is clickable. */
export function SwitchRow({
  checked,
  onChange,
  title,
  hint,
  disabled,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  title: ReactNode
  hint?: ReactNode
  disabled?: boolean
}) {
  const id = useId()
  return (
    <label className="switch-row" htmlFor={id}>
      <Switch id={id} checked={checked} onChange={onChange} disabled={disabled} accent />
      <span className="stack" style={{ gap: 2 }}>
        <span className="strong" style={{ fontSize: 13.5, fontWeight: 560 }}>
          {title}
        </span>
        {hint && <span className="field-hint">{hint}</span>}
      </span>
    </label>
  )
}

/* ---------------- Checkbox ---------------- */
export function Checkbox({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean | 'indeterminate'
  onChange: (v: boolean) => void
  label?: string
  disabled?: boolean
}) {
  return (
    <CheckboxPrimitive.Root
      className="checkbox"
      checked={checked}
      onCheckedChange={(v) => onChange(v === true)}
      aria-label={label}
      disabled={disabled}
      onClick={(e) => e.stopPropagation()}
    >
      <CheckboxPrimitive.Indicator>
        {checked === 'indeterminate' ? <Minus strokeWidth={3} /> : <Check strokeWidth={3} />}
      </CheckboxPrimitive.Indicator>
    </CheckboxPrimitive.Root>
  )
}

/* ---------------- Select ---------------- */
export interface SelectOption<T extends string = string> {
  value: T
  label: ReactNode
  hint?: ReactNode
}

export function Select<T extends string>({
  value,
  onChange,
  options,
  placeholder,
  prefix,
  size,
  className,
  style,
  ariaLabel,
  disabled,
}: {
  value: T
  onChange: (v: T) => void
  options: SelectOption<T>[]
  placeholder?: string
  prefix?: ReactNode
  size?: 'sm'
  className?: string
  style?: React.CSSProperties
  ariaLabel?: string
  disabled?: boolean
}) {
  return (
    <SelectPrimitive.Root value={value} onValueChange={(v) => onChange(v as T)} disabled={disabled}>
      <SelectPrimitive.Trigger
        className={clsx('select-trigger', size === 'sm' && 'select-trigger-sm', className)}
        style={style}
        aria-label={ariaLabel}
      >
        <span className="row" style={{ gap: 7, minWidth: 0 }}>
          {prefix && <span className="select-prefix">{prefix}</span>}
          <span className="ellipsis">
            <SelectPrimitive.Value placeholder={placeholder} />
          </span>
        </span>
        <SelectPrimitive.Icon asChild>
          <ChevronDown className="select-chevron" />
        </SelectPrimitive.Icon>
      </SelectPrimitive.Trigger>
      <SelectPrimitive.Portal>
        <SelectPrimitive.Content className="popover" position="popper" sideOffset={6} align="start" style={{ minWidth: 'var(--radix-select-trigger-width)' }}>
          <SelectPrimitive.Viewport>
            {options.map((o) => (
              <SelectPrimitive.Item key={o.value} value={o.value} className="popover-item">
                <span className="stack" style={{ gap: 1 }}>
                  <SelectPrimitive.ItemText>{o.label}</SelectPrimitive.ItemText>
                  {o.hint && <span className="xs faint">{o.hint}</span>}
                </span>
                <SelectPrimitive.ItemIndicator className="item-check">
                  <Check size={14} />
                </SelectPrimitive.ItemIndicator>
              </SelectPrimitive.Item>
            ))}
          </SelectPrimitive.Viewport>
        </SelectPrimitive.Content>
      </SelectPrimitive.Portal>
    </SelectPrimitive.Root>
  )
}

/* ---------------- Segmented ---------------- */
export function Segmented<T extends string>({
  value,
  onChange,
  items,
  id,
  size,
}: {
  value: T
  onChange: (v: T) => void
  items: { value: T; label: ReactNode; count?: number; icon?: ReactNode }[]
  /** unique per control, drives the sliding pill */
  id: string
  size?: 'sm'
}) {
  return (
    <div className="segmented" role="tablist" style={size === 'sm' ? { padding: 2 } : undefined}>
      {items.map((it) => {
        const active = it.value === value
        return (
          <button
            key={it.value}
            type="button"
            role="tab"
            aria-selected={active}
            data-active={active}
            className="segmented-item"
            onClick={() => onChange(it.value)}
          >
            {active && (
              <motion.span
                layoutId={`seg-${id}`}
                className="segmented-pill"
                transition={{ type: 'spring', stiffness: 520, damping: 40, mass: 0.8 }}
              />
            )}
            {it.icon}
            {it.label}
            {it.count != null && <span className="seg-count">{it.count}</span>}
          </button>
        )
      })}
    </div>
  )
}

/* ---------------- Tooltip ---------------- */
export function Tip({ content, children, side = 'top' }: { content: ReactNode; children: ReactNode; side?: 'top' | 'bottom' | 'left' | 'right' }) {
  if (content == null || content === '') return <>{children}</>
  return (
    <TooltipPrimitive.Root delayDuration={250}>
      <TooltipPrimitive.Trigger asChild>{children}</TooltipPrimitive.Trigger>
      <TooltipPrimitive.Portal>
        <TooltipPrimitive.Content className="tooltip" side={side} sideOffset={6} collisionPadding={8}>
          {content}
        </TooltipPrimitive.Content>
      </TooltipPrimitive.Portal>
    </TooltipPrimitive.Root>
  )
}

export const TooltipProvider = TooltipPrimitive.Provider

/* ---------------- Badge / Dot / Bar ---------------- */
export type Tone = 'neutral' | 'accent' | 'ok' | 'warn' | 'bad' | 'pink' | 'teal' | 'outline' | 'solid-bad'

export function Badge({ tone = 'neutral', children, icon, title }: { tone?: Tone; children: ReactNode; icon?: ReactNode; title?: string }) {
  return (
    <span className={clsx('badge', tone !== 'neutral' && `badge-${tone}`)} title={title}>
      {icon}
      {children}
    </span>
  )
}

export function Dot({ tone, live }: { tone: 'ok' | 'warn' | 'bad' | 'off'; live?: boolean }) {
  return <span className={clsx('dot', `dot-${tone}`, live && 'dot-live')} aria-hidden />
}

export function Bar({ value, tone, thin }: { value: number; tone?: 'ok' | 'warn' | 'bad' | 'accent'; thin?: boolean }) {
  const pct = Math.max(0, Math.min(100, value * 100))
  return (
    <div className={clsx('bar', tone && tone !== 'accent' && `bar-${tone}`, thin && 'bar-thin')} role="progressbar" aria-valuenow={Math.round(pct)} aria-valuemin={0} aria-valuemax={100}>
      <span style={{ width: `${pct}%` }} />
    </div>
  )
}

/** Quota usage colour: calm until 70 %, amber to 90 %, red beyond. */
export function usageTone(ratio: number): 'ok' | 'warn' | 'bad' {
  if (ratio > 0.9) return 'bad'
  if (ratio > 0.7) return 'warn'
  return 'ok'
}
