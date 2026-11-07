import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { useEffect, useState } from "react"
import { useForm } from "react-hook-form"
import { z } from "zod"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel, FieldSeparator } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import { api } from "@/lib/api"
import { keys, queryClient } from "@/lib/queries"

const schema = z
  .object({
    username: z.string().trim().min(1, "이름을 입력하세요").max(64, "이름은 64자 이하여야 합니다"),
    current_password: z.string().min(1, "현재 비밀번호를 입력하세요"),
    new_password: z.string().refine((v) => v === "" || v.length >= 8, "새 비밀번호는 8자 이상이어야 합니다"),
    confirm: z.string(),
  })
  .refine((v) => v.new_password === v.confirm, { path: ["confirm"], message: "새 비밀번호와 같게 입력하세요" })

type Values = z.infer<typeof schema>

export default function ProfilePage() {
  const profile = useQuery({ queryKey: ["profile"], queryFn: api.profile })
  const [error, setError] = useState<string | null>(null)
  const form = useForm<Values>({ resolver: zodResolver(schema), defaultValues: { username: "", current_password: "", new_password: "", confirm: "" } })

  useEffect(() => {
    if (profile.data) form.reset({ username: profile.data.username, current_password: "", new_password: "", confirm: "" })
  }, [profile.data, form])

  const submit = form.handleSubmit(async (v) => {
    setError(null)
    try {
      const p = await api.updateProfile({ username: v.username, current_password: v.current_password, new_password: v.new_password })
      queryClient.setQueryData(["profile"], p)
      queryClient.invalidateQueries({ queryKey: keys.me })
      form.reset({ username: p.username, current_password: "", new_password: "", confirm: "" })
      toast.add({
        title: "프로필을 저장했습니다",
        description: v.new_password ? "비밀번호를 바꿔 다른 기기의 로그인은 모두 끝났습니다." : undefined,
        type: "success",
      })
    } catch (e) {
      setError((e as Error).message)
    }
  })

  const e = form.formState.errors
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-xl font-semibold">프로필</h1>
        <p className="text-sm text-muted-foreground">사이트에 들어올 때 쓰는 관리자 이름과 비밀번호입니다.</p>
      </div>
      {profile.data?.is_default_password ? (
        <Alert variant="destructive">
          <AlertDescription>아직 초기 비밀번호(1234)입니다. 다른 사람이 들어올 수 있으니 바로 바꾸세요.</AlertDescription>
        </Alert>
      ) : null}
      <form onSubmit={submit} className="max-w-md" noValidate>
        <FieldGroup>
          <Field data-invalid={e.username ? true : undefined}>
            <FieldLabel htmlFor="username">관리자 이름</FieldLabel>
            <Input id="username" autoComplete="username" aria-invalid={!!e.username} {...form.register("username")} />
            <FieldError errors={[e.username]} />
          </Field>
          <FieldSeparator />
          <Field data-invalid={e.new_password ? true : undefined}>
            <FieldLabel htmlFor="new_password">새 비밀번호</FieldLabel>
            <Input id="new_password" type="password" autoComplete="new-password" aria-invalid={!!e.new_password} {...form.register("new_password")} />
            <FieldDescription>바꾸지 않으려면 비워 두세요. 8자 이상.</FieldDescription>
            <FieldError errors={[e.new_password]} />
          </Field>
          <Field data-invalid={e.confirm ? true : undefined}>
            <FieldLabel htmlFor="confirm">새 비밀번호 확인</FieldLabel>
            <Input id="confirm" type="password" autoComplete="new-password" aria-invalid={!!e.confirm} {...form.register("confirm")} />
            <FieldError errors={[e.confirm]} />
          </Field>
          <FieldSeparator />
          <Field data-invalid={e.current_password ? true : undefined}>
            <FieldLabel htmlFor="current_password">현재 비밀번호</FieldLabel>
            <Input id="current_password" type="password" autoComplete="current-password" aria-invalid={!!e.current_password} {...form.register("current_password")} />
            <FieldDescription>변경 내용을 저장하려면 현재 비밀번호가 필요합니다.</FieldDescription>
            <FieldError errors={[e.current_password]} />
          </Field>
          {error ? (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          ) : null}
          <div>
            <Button type="submit" disabled={form.formState.isSubmitting}>
              {form.formState.isSubmitting ? <Spinner data-icon="inline-start" /> : null}
              프로필 저장
            </Button>
          </div>
        </FieldGroup>
      </form>
    </div>
  )
}
