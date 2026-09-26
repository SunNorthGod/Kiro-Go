import { forwardRef, useState, type InputHTMLAttributes, type ReactNode, type TextareaHTMLAttributes } from 'react'
import clsx from 'clsx'
import { Eye, EyeOff } from 'lucide-react'

export function Field({
  label,
  hint,
  error,
  children,
  className,
  htmlFor,
  aside,
}: {
  label?: ReactNode
  hint?: ReactNode
  error?: ReactNode
  children: ReactNode
  className?: string
  htmlFor?: string
  aside?: ReactNode
}) {
  return (
    <div className={clsx('field', className)}>
      {(label || aside) && (
        <div className="row row-between">
          {label && (
            <label className="field-label" htmlFor={htmlFor}>
              {label}
            </label>
          )}
          {aside}
        </div>
      )}
      {children}
      {error ? <div className="field-error">{error}</div> : hint ? <div className="field-hint">{hint}</div> : null}
    </div>
  )
}

export interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  icon?: ReactNode
  suffix?: ReactNode
  mono?: boolean
  invalid?: boolean
}

export const Input = forwardRef<HTMLInputElement, InputProps>(function Input(
  { icon, suffix, mono, invalid, className, ...rest },
  ref,
) {
  const input = (
    <input
      ref={ref}
      className={clsx('input', mono && 'input-mono', className)}
      aria-invalid={invalid || undefined}
      {...rest}
    />
  )
  if (!icon && !suffix) return input
  return (
    <div className="input-group">
      {icon && <span className="input-icon">{icon}</span>}
      {input}
      {suffix && <span className="input-suffix">{suffix}</span>}
    </div>
  )
})

export const PasswordInput = forwardRef<HTMLInputElement, Omit<InputProps, 'type' | 'suffix'>>(function PasswordInput(
  props,
  ref,
) {
  const [shown, setShown] = useState(false)
  return (
    <Input
      ref={ref}
      {...props}
      type={shown ? 'text' : 'password'}
      suffix={
        <button
          type="button"
          className="btn btn-ghost btn-sm btn-icon"
          onClick={() => setShown((s) => !s)}
          aria-label={shown ? '隐藏' : '显示'}
          tabIndex={-1}
        >
          {shown ? <EyeOff /> : <Eye />}
        </button>
      }
    />
  )
})

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement>>(
  function Textarea({ className, ...rest }, ref) {
    return <textarea ref={ref} className={clsx('input', className)} {...rest} />
  },
)
