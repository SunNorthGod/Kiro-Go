import { type ReactNode } from 'react'
import { AlertDialog as AlertPrimitive, Dialog as DialogPrimitive, DropdownMenu as MenuPrimitive } from 'radix-ui'
import { X } from 'lucide-react'
import clsx from 'clsx'
import { Button } from './Button'
import { settleConfirm, useConfirmState } from '@/lib/confirm'

export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  width = 520,
  children,
  footer,
  bodyClassName,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: ReactNode
  description?: ReactNode
  width?: number
  children: ReactNode
  footer?: ReactNode
  bodyClassName?: string
}) {
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="dialog-overlay" />
        <DialogPrimitive.Content
          className="dialog"
          style={{ ['--dialog-w' as string]: `${width}px` }}
          onOpenAutoFocus={(e) => {
            // Focus the first form control rather than the close button.
            // With no form field, focus the panel itself so no button tooltip pops open.
            const root = e.currentTarget as HTMLElement
            const first = root.querySelector<HTMLElement>('[data-autofocus], .dialog-body input:not([readonly]), .dialog-body textarea')
            e.preventDefault()
            ;(first || root).focus({ preventScroll: true })
          }}
        >
          <div className="dialog-header">
            <div className="grow">
              <DialogPrimitive.Title className="dialog-title">{title}</DialogPrimitive.Title>
              {description ? (
                <DialogPrimitive.Description className="dialog-desc">{description}</DialogPrimitive.Description>
              ) : (
                <DialogPrimitive.Description className="sr-only">{typeof title === 'string' ? title : ''}</DialogPrimitive.Description>
              )}
            </div>
            <DialogPrimitive.Close asChild>
              <Button variant="ghost" size="sm" iconOnly icon={<X />} className="dialog-close">
                关闭
              </Button>
            </DialogPrimitive.Close>
          </div>
          <div className={clsx('dialog-body', bodyClassName)}>{children}</div>
          {footer && <div className="dialog-footer">{footer}</div>}
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  )
}

/** Single host for the promise-based confirm(). Mount once per app. */
export function ConfirmHost() {
  const s = useConfirmState()
  return (
    <AlertPrimitive.Root open={s.open} onOpenChange={(o) => !o && settleConfirm(false)}>
      <AlertPrimitive.Portal>
        <AlertPrimitive.Overlay className="dialog-overlay" />
        <AlertPrimitive.Content className="dialog" style={{ ['--dialog-w' as string]: '420px' }}>
          <div className="dialog-header">
            <div className="grow">
              <AlertPrimitive.Title className="dialog-title">{s.title}</AlertPrimitive.Title>
              {s.description ? (
                <AlertPrimitive.Description className="dialog-desc" style={{ fontSize: 13, color: 'var(--text-2)' }}>
                  {s.description}
                </AlertPrimitive.Description>
              ) : (
                <AlertPrimitive.Description className="sr-only">{s.title}</AlertPrimitive.Description>
              )}
            </div>
          </div>
          <div className="dialog-footer" style={{ borderTop: 0, background: 'transparent', paddingTop: 6 }}>
            <AlertPrimitive.Cancel asChild>
              <Button variant="secondary" onClick={() => settleConfirm(false)}>
                {s.cancelText || '取消'}
              </Button>
            </AlertPrimitive.Cancel>
            <AlertPrimitive.Action asChild>
              <Button variant={s.danger ? 'danger' : 'primary'} onClick={() => settleConfirm(true)}>
                {s.confirmText || '确定'}
              </Button>
            </AlertPrimitive.Action>
          </div>
        </AlertPrimitive.Content>
      </AlertPrimitive.Portal>
    </AlertPrimitive.Root>
  )
}

/* ---------------- Dropdown menu ---------------- */
export const Menu = MenuPrimitive.Root
export const MenuTrigger = MenuPrimitive.Trigger

export function MenuContent({ children, align = 'end' }: { children: ReactNode; align?: 'start' | 'end' | 'center' }) {
  return (
    <MenuPrimitive.Portal>
      <MenuPrimitive.Content className="popover" align={align} sideOffset={6} collisionPadding={8}>
        {children}
      </MenuPrimitive.Content>
    </MenuPrimitive.Portal>
  )
}

export function MenuItem({
  icon,
  children,
  onSelect,
  danger,
  disabled,
}: {
  icon?: ReactNode
  children: ReactNode
  onSelect?: () => void
  danger?: boolean
  disabled?: boolean
}) {
  return (
    <MenuPrimitive.Item className={clsx('popover-item', danger && 'danger')} onSelect={onSelect} disabled={disabled}>
      {icon}
      {children}
    </MenuPrimitive.Item>
  )
}

export function MenuSeparator() {
  return <MenuPrimitive.Separator className="popover-sep" />
}

export function MenuLabel({ children }: { children: ReactNode }) {
  return <MenuPrimitive.Label className="popover-label">{children}</MenuPrimitive.Label>
}
