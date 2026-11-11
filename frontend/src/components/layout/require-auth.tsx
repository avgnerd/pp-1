import { useRouter } from "@tanstack/react-router"
import { useEffect, type ReactNode } from "react"

import { keys, queryClient } from "@/lib/queries"

/**
 * The redirect to /login happens in the route's beforeLoad. This component only reacts to a
 * session that expires while the app is open: it forgets the session and re-runs the guard.
 */
export function RequireAuth({ children }: { children: ReactNode }) {
  const router = useRouter()
  useEffect(() => {
    const onUnauthorized = () => {
      queryClient.setQueryData(keys.me, { authenticated: false })
      router.invalidate()
    }
    window.addEventListener("dvfs:unauthorized", onUnauthorized)
    return () => window.removeEventListener("dvfs:unauthorized", onUnauthorized)
  }, [router])
  return <>{children}</>
}
