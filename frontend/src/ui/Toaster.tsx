import { Toaster as Sonner } from 'sonner'
import { CircleAlert, CircleCheck, Info, TriangleAlert } from 'lucide-react'
import { useEffect, useState } from 'react'

export function Toaster() {
  const [mobile, setMobile] = useState(() => window.matchMedia('(max-width: 640px)').matches)
  useEffect(() => {
    const mq = window.matchMedia('(max-width: 640px)')
    const on = () => setMobile(mq.matches)
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])
  return (
    <Sonner
      position={mobile ? 'top-center' : 'bottom-right'}
      gap={8}
      offset={20}
      visibleToasts={4}
      toastOptions={{ unstyled: true, classNames: { toast: 'toast' }, duration: 3200 }}
      icons={{
        success: <CircleCheck size={16} />,
        error: <CircleAlert size={16} />,
        warning: <TriangleAlert size={16} />,
        info: <Info size={16} />,
      }}
    />
  )
}
