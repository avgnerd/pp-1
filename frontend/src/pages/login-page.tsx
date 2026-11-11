import { zodResolver } from "@hookform/resolvers/zod"
import { useNavigate, useSearch } from "@tanstack/react-router"
import { CheckCircle2Icon, XIcon } from "lucide-react"
import { useState } from "react"
import { useForm } from "react-hook-form"
import { z } from "zod"

import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { Alert, AlertAction, AlertDescription } from "@/components/ui/alert"
import { api } from "@/lib/api"
import { keys, queryClient } from "@/lib/queries"

const schema = z.object({
  username: z.string().trim().min(1, "이름을 입력하세요"),
  password: z.string().min(1, "비밀번호를 입력하세요"),
})
type Values = z.infer<typeof schema>

export function LoginPage() {
  const navigate = useNavigate()
  const { next, restored } = useSearch({ from: "/login" })
  const [error, setError] = useState<string | null>(null)
  const [dismissedRestored, setDismissedRestored] = useState(false)
  const form = useForm<Values>({ resolver: zodResolver(schema), defaultValues: { username: "", password: "" } })

  const showRestoredBanner =
    !dismissedRestored &&
    (Boolean(restored) || (typeof window !== "undefined" && new URLSearchParams(window.location.search).get("restored") !== null))

  const onSubmit = form.handleSubmit(async (v) => {
    setError(null)
    setDismissedRestored(true)
    try {
      const me = await api.login(v.username, v.password)
      queryClient.setQueryData(keys.me, me)
      const dest = next && next.startsWith("/") && !next.startsWith("//") && !next.startsWith("/login") ? next : "/"
      navigate({ href: dest, replace: true })
    } catch (e) {
      setError((e as Error).message)
    }
  })

  const { errors, isSubmitting } = form.formState

  return (
    <main className="grid min-h-svh place-items-center px-4">
      <div className="flex w-full max-w-sm flex-col gap-8">
        <div className="flex items-center gap-3">
          <img src="/favicon.svg" alt="" className="size-9" />
          <div>
            <h1 className="text-lg font-semibold">Dropbox VFS</h1>
            <p className="text-sm text-muted-foreground">관리자 계정으로 로그인하세요</p>
          </div>
        </div>
        {showRestoredBanner ? (
          <Alert>
            <CheckCircle2Icon />
            <AlertDescription>복원이 끝났습니다. 백업에 저장된 관리자 계정으로 다시 로그인하세요</AlertDescription>
            <AlertAction>
              <Button
                type="button"
                variant="ghost"
                size="icon-xs"
                aria-label="알림 닫기"
                onClick={() => setDismissedRestored(true)}
              >
                <XIcon />
              </Button>
            </AlertAction>
          </Alert>
        ) : null}
        <form onSubmit={onSubmit} noValidate>
          <FieldGroup>
            <Field data-invalid={errors.username ? true : undefined}>
              <FieldLabel htmlFor="username">이름</FieldLabel>
              <Input id="username" autoComplete="username" autoFocus aria-invalid={!!errors.username} {...form.register("username")} />
              <FieldError errors={[errors.username]} />
            </Field>
            <Field data-invalid={errors.password ? true : undefined}>
              <FieldLabel htmlFor="password">비밀번호</FieldLabel>
              <Input id="password" type="password" autoComplete="current-password" aria-invalid={!!errors.password} {...form.register("password")} />
              <FieldError errors={[errors.password]} />
            </Field>
            {error ? (
              <Alert variant="destructive">
                <AlertDescription>{error}</AlertDescription>
              </Alert>
            ) : null}
            <Button type="submit" disabled={isSubmitting}>
              {isSubmitting ? <Spinner data-icon="inline-start" /> : null}
              로그인
            </Button>
          </FieldGroup>
        </form>
      </div>
    </main>
  )
}
