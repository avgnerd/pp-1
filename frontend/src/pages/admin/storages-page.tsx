import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { useVirtualizer } from "@tanstack/react-virtual"
import { CheckCircle2Icon, CopyIcon, ExternalLinkIcon, KeyRoundIcon, PencilIcon, PlusIcon, RefreshCwIcon, Trash2Icon } from "lucide-react"
import { useEffect, useMemo, useRef, useState } from "react"
import { Controller, useForm, useWatch } from "react-hook-form"
import { z } from "zod"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupButton, InputGroupInput } from "@/components/ui/input-group"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { api, ApiError, type AuthStart, type Storage } from "@/lib/api"
import { copyText } from "@/lib/paths"
import { keys, queryClient } from "@/lib/queries"

const APP_CONSOLE = "https://www.dropbox.com/developers/apps"

function refreshAll() {
  queryClient.invalidateQueries({ queryKey: ["storages"] })
  queryClient.invalidateQueries({ queryKey: keys.mounts })
}

export default function StoragesPage() {
  const storages = useQuery({ queryKey: ["storages"], queryFn: api.storages })
  const info = useQuery({ queryKey: ["oauth-info"], queryFn: api.oauthInfo })
  const [adding, setAdding] = useState(false)
  const [editing, setEditing] = useState<Storage | null>(null)
  const [deleting, setDeleting] = useState<Storage | null>(null)
  const [auth, setAuth] = useState<AuthStart | null>(null)
  const [reauthFor, setReauthFor] = useState<Storage | null>(null)

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">저장소</h1>
          <p className="text-sm text-muted-foreground">Dropbox 계정마다 저장소 하나를 만들고 가상 마운트 경로에 연결합니다.</p>
        </div>
        <Button onClick={() => setAdding(true)}>
          <PlusIcon data-icon="inline-start" />
          저장소 추가
        </Button>
      </div>

      <RedirectHelp localhost={info.data?.localhost_redirect} publicRedirect={info.data?.public_redirect} scopes={info.data?.scopes ?? []} />
      <PasteUrlCard />

      {storages.isPending ? (
        <Spinner />
      ) : storages.isError ? (
        <Alert variant="destructive">
          <AlertDescription>{(storages.error as Error).message}</AlertDescription>
        </Alert>
      ) : storages.data.length === 0 ? (
        <p className="text-sm text-muted-foreground">아직 저장소가 없습니다. '저장소 추가'로 첫 Dropbox 계정을 연결하세요.</p>
      ) : (
        <StorageList items={storages.data} onEdit={setEditing} onDelete={setDeleting} onReauth={setReauthFor} />
      )}

      <AddStorageDialog
        open={adding}
        onClose={() => setAdding(false)}
        onStarted={(a) => {
          setAdding(false)
          setAuth(a)
          refreshAll()
        }}
      />
      <EditStorageDialog storage={editing} onClose={() => setEditing(null)} />
      <DeleteStorageDialog storage={deleting} onClose={() => setDeleting(null)} />
      <ReauthDialog
        storage={reauthFor}
        hasPublic={!!info.data?.public_redirect}
        onClose={() => setReauthFor(null)}
        onStarted={(a) => {
          setReauthFor(null)
          setAuth(a)
        }}
      />
      <AuthDialog start={auth} onClose={() => setAuth(null)} />
    </div>
  )
}

function CopyField({ value, label }: { value: string; label: string }) {
  return (
    <InputGroup>
      <InputGroupInput readOnly value={value} aria-label={label} onFocus={(e) => e.currentTarget.select()} />
      <InputGroupAddon align="inline-end">
        <InputGroupButton
          aria-label={`${label} 복사`}
          onClick={async () => {
            const ok = await copyText(value)
            toast.add({ title: ok ? "복사했습니다" : "복사하지 못했습니다. 직접 선택해서 복사하세요", type: ok ? "success" : "error", timeout: 2000 })
          }}
        >
          <CopyIcon />
        </InputGroupButton>
      </InputGroupAddon>
    </InputGroup>
  )
}

function RedirectHelp({ localhost, publicRedirect, scopes }: { localhost?: string; publicRedirect?: string; scopes: string[] }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Dropbox 앱 준비</CardTitle>
        <CardDescription>
          저장소마다 쓸 Dropbox 앱을{" "}
          <a href={APP_CONSOLE} target="_blank" rel="noreferrer" className="text-primary underline underline-offset-4">
            App Console
          </a>
          에서 만들고(Scoped access, Full Dropbox), Permissions 탭에서 아래 권한을 켠 뒤 Settings 탭의 OAuth 2 Redirect URIs에 아래 주소를 추가하세요.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <FieldGroup>
          <Field>
            <FieldLabel>localhost Redirect URI (기본)</FieldLabel>
            {localhost ? <CopyField value={localhost} label="localhost Redirect URI" /> : <Spinner />}
            <FieldDescription>
              이 PC에서 사이트를 열었거나 SSH 터널(ssh -L)로 접속했다면 승인 즉시 연결됩니다. 서버 IP로 접속했다면 승인 뒤 뜨는 연결 오류 페이지의 주소를 아래 칸에 붙여넣으세요.
            </FieldDescription>
          </Field>
          {publicRedirect ? (
            <Field>
              <FieldLabel>public_url Redirect URI</FieldLabel>
              <CopyField value={publicRedirect} label="public_url Redirect URI" />
            </Field>
          ) : null}
        </FieldGroup>
        <div className="flex flex-wrap gap-1.5">
          {scopes.map((s) => (
            <Badge key={s} variant="secondary">
              {s}
            </Badge>
          ))}
        </div>
      </CardContent>
    </Card>
  )
}

function PasteUrlCard() {
  const [url, setUrl] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const s = await api.pasteUrl(url)
      setUrl("")
      refreshAll()
      toast.add({ title: "Dropbox 계정을 연결했습니다", description: `${s.account_email} → ${s.mount_path}`, type: "success" })
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit}>
      <Field data-invalid={error ? true : undefined}>
        <FieldLabel htmlFor="paste-url">승인 후 이동한 주소 붙여넣기</FieldLabel>
        <InputGroup>
          <InputGroupInput id="paste-url" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="http://localhost:…/api/oauth/callback?code=…&state=…" aria-invalid={!!error} />
          <InputGroupAddon align="inline-end">
            <InputGroupButton type="submit" variant="default" disabled={busy || !url.trim()}>
              {busy ? <Spinner data-icon="inline-start" /> : null}
              연결 완료
            </InputGroupButton>
          </InputGroupAddon>
        </InputGroup>
        <FieldError errors={error ? [{ message: error }] : undefined} />
      </Field>
    </form>
  )
}

function StatusBadge({ s }: { s: Storage }) {
  if (s.status === "active") return <Badge variant="secondary">연결됨</Badge>
  if (s.status === "error") return <Badge variant="destructive">재인증 필요</Badge>
  return <Badge variant="outline">인증 대기</Badge>
}

/** Every storage, scrollable as one list (virtualized for 100+ accounts). */
function StorageList({ items, onEdit, onDelete, onReauth }: { items: Storage[]; onEdit: (s: Storage) => void; onDelete: (s: Storage) => void; onReauth: (s: Storage) => void }) {
  const parentRef = useRef<HTMLDivElement>(null)
  const v = useVirtualizer({ count: items.length, getScrollElement: () => parentRef.current, estimateSize: () => 76, overscan: 8 })
  return (
    <div className="flex flex-col gap-2">
      <p className="text-sm text-muted-foreground tabular-nums">{items.length.toLocaleString()}개 저장소</p>
      <div ref={parentRef} className="max-h-[65svh] overflow-auto rounded-lg border" role="list" aria-label="저장소 목록">
        <div style={{ height: v.getTotalSize(), position: "relative" }}>
          {v.getVirtualItems().map((it) => {
            const s = items[it.index]
            return (
              <div
                key={s.id}
                role="listitem"
                ref={v.measureElement}
                data-index={it.index}
                className="absolute inset-x-0 flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-border/60 px-3 py-3"
                style={{ transform: `translateY(${it.start}px)` }}
              >
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="font-medium break-all">{s.mount_path}</span>
                    <StatusBadge s={s} />
                    {s.name ? <span className="text-sm text-muted-foreground">{s.name}</span> : null}
                  </div>
                  <p className="text-sm text-muted-foreground break-all">
                    {s.account_email || "계정 미연결"}, Dropbox 경로 {s.root_path}, 캐시 {s.cache_ttl_min}분
                  </p>
                  {s.last_error ? <p className="text-sm text-destructive">{s.last_error}</p> : null}
                </div>
                <div className="flex items-center gap-1">
                  <Button variant="ghost" size="sm" onClick={() => onReauth(s)}>
                    <RefreshCwIcon data-icon="inline-start" />
                    {s.status === "pending" ? "인증 계속" : "재인증"}
                  </Button>
                  <Button variant="ghost" size="icon-sm" aria-label={`${s.mount_path} 편집`} onClick={() => onEdit(s)}>
                    <PencilIcon />
                  </Button>
                  <Button variant="ghost" size="icon-sm" aria-label={`${s.mount_path} 삭제`} onClick={() => onDelete(s)} className="text-destructive">
                    <Trash2Icon />
                  </Button>
                </div>
              </div>
            )
          })}
        </div>
      </div>
    </div>
  )
}

const mountSchema = z
  .string()
  .trim()
  .min(1, "마운트 경로를 입력하세요 (예: /path/to)")
  .refine((v) => v.startsWith("/"), "마운트 경로는 /로 시작해야 합니다 (예: /path/to)")
  .refine((v) => v !== "/", "루트(/)는 마운트 경로로 쓸 수 없습니다")

const baseSchema = z.object({
  name: z.string().max(100),
  mount_path: mountSchema,
  root_path: z.string().trim().refine((v) => v === "" || v.startsWith("/"), "Dropbox 경로는 /로 시작해야 합니다"),
  cache_ttl_min: z.number({ message: "숫자로 입력하세요" }).int().min(1, "1분 이상이어야 합니다").max(10080, "7일(10080분) 이하여야 합니다"),
})
const addSchema = baseSchema.extend({
  app_key: z.string().trim().min(1, "App key를 입력하세요"),
  app_secret: z.string().trim().min(1, "App secret을 입력하세요"),
  auth_mode: z.enum(["localhost", "public", "code"]),
})
type AddValues = z.infer<typeof addSchema>
type EditValues = z.infer<typeof baseSchema>

/** Debounced server check for mount conflicts while typing. */
function useMountCheck(value: string, id: number) {
  const [result, setResult] = useState<{ value: string; msg: string | null } | null>(null)
  useEffect(() => {
    if (!value || !value.startsWith("/") || value === "/") return
    const t = setTimeout(() => {
      api.checkMount(value, id).then(
        () => setResult({ value, msg: null }),
        (e) => setResult({ value, msg: e instanceof ApiError ? e.message : null }),
      )
    }, 350)
    return () => clearTimeout(t)
  }, [value, id])
  // Only a result for the current input counts.
  return result && result.value === value ? result.msg : null
}

function AddStorageDialog({ open, onClose, onStarted }: { open: boolean; onClose: () => void; onStarted: (a: AuthStart) => void }) {
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">{open ? <AddStorageForm onClose={onClose} onStarted={onStarted} /> : null}</DialogContent>
    </Dialog>
  )
}

function AddStorageForm({ onClose, onStarted }: { onClose: () => void; onStarted: (a: AuthStart) => void }) {
  const info = useQuery({ queryKey: ["oauth-info"], queryFn: api.oauthInfo })
  const form = useForm<AddValues>({
    resolver: zodResolver(addSchema),
    defaultValues: { name: "", mount_path: "", root_path: "/", app_key: "", app_secret: "", cache_ttl_min: 30, auth_mode: "localhost" },
  })
  const [error, setError] = useState<string | null>(null)
  const mount = useWatch({ control: form.control, name: "mount_path" })
  const authMode = useWatch({ control: form.control, name: "auth_mode" })
  const mountMsg = useMountCheck(mount, 0)

  const submit = form.handleSubmit(async (v) => {
    setError(null)
    try {
      onStarted(await api.createStorage(v))
    } catch (e) {
      if (e instanceof ApiError && e.field && e.field in v) form.setError(e.field as keyof AddValues, { message: e.message })
      else setError((e as Error).message)
    }
  })
  const e = form.formState.errors
  const mountError = e.mount_path ?? (mountMsg ? { message: mountMsg } : undefined)
  return (
        <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>저장소 추가</DialogTitle>
            <DialogDescription>저장한 뒤 Dropbox 로그인 화면에서 이 앱의 접근을 승인하면 연결됩니다.</DialogDescription>
          </DialogHeader>
          <FieldGroup className="max-h-[60svh] overflow-y-auto pr-1">
            <Field data-invalid={mountError ? true : undefined}>
              <FieldLabel htmlFor="mount_path">마운트 경로</FieldLabel>
              <Input id="mount_path" placeholder="/path/to" aria-invalid={!!mountError} {...form.register("mount_path")} />
              <FieldDescription>사이트에서 이 계정이 보일 가상 경로입니다. 다른 저장소의 상위·하위 경로는 쓸 수 없습니다.</FieldDescription>
              <FieldError errors={[mountError]} />
            </Field>
            <Field data-invalid={e.root_path ? true : undefined}>
              <FieldLabel htmlFor="root_path">Dropbox 원본 경로</FieldLabel>
              <Input id="root_path" placeholder="/" aria-invalid={!!e.root_path} {...form.register("root_path")} />
              <FieldDescription>계정 전체는 /, 특정 폴더만 보이려면 /Videos/2024처럼 입력하세요.</FieldDescription>
              <FieldError errors={[e.root_path]} />
            </Field>
            <Field>
              <FieldLabel htmlFor="name">이름 (선택)</FieldLabel>
              <Input id="name" placeholder="예: 영화 계정" {...form.register("name")} />
            </Field>
            <Field data-invalid={e.app_key ? true : undefined}>
              <FieldLabel htmlFor="app_key">App key</FieldLabel>
              <Input id="app_key" autoComplete="off" aria-invalid={!!e.app_key} {...form.register("app_key")} />
              <FieldError errors={[e.app_key]} />
            </Field>
            <Field data-invalid={e.app_secret ? true : undefined}>
              <FieldLabel htmlFor="app_secret">App secret</FieldLabel>
              <Input id="app_secret" type="password" autoComplete="off" aria-invalid={!!e.app_secret} {...form.register("app_secret")} />
              <FieldDescription>서버 비밀키로 암호화해 저장합니다.</FieldDescription>
              <FieldError errors={[e.app_secret]} />
            </Field>
            <Field data-invalid={e.cache_ttl_min ? true : undefined}>
              <FieldLabel htmlFor="cache_ttl_min">캐시 만료 시간 (분)</FieldLabel>
              <Input id="cache_ttl_min" type="number" className="w-32" aria-invalid={!!e.cache_ttl_min} {...form.register("cache_ttl_min", { valueAsNumber: true })} />
              <FieldDescription>폴더 목록과 폴더 크기를 이 시간 동안 기억합니다. Dropbox에서 바뀐 내용은 이 시간이 지나면 반영됩니다.</FieldDescription>
              <FieldError errors={[e.cache_ttl_min]} />
            </Field>
            <Field>
              <FieldLabel>인증 방식</FieldLabel>
              <Controller
                control={form.control}
                name="auth_mode"
                render={({ field }) => (
                  <ToggleGroup value={[field.value]} onValueChange={(v) => v[0] && field.onChange(v[0])} variant="outline" spacing={0} aria-label="인증 방식">
                    <ToggleGroupItem value="localhost">localhost</ToggleGroupItem>
                    <ToggleGroupItem value="public" disabled={!info.data?.public_redirect}>
                      public_url
                    </ToggleGroupItem>
                    <ToggleGroupItem value="code">코드 붙여넣기</ToggleGroupItem>
                  </ToggleGroup>
                )}
              />
              <FieldDescription>
                {authMode === "code"
                  ? "Redirect URI 없이 승인하고, Dropbox 화면에 표시된 코드를 붙여넣습니다."
                  : authMode === "public"
                    ? `승인 후 ${info.data?.public_redirect}로 돌아옵니다.`
                    : `승인 후 ${info.data?.localhost_redirect ?? "localhost"}로 돌아옵니다. 이 주소가 App Console에 등록되어 있어야 합니다.`}
              </FieldDescription>
            </Field>
          </FieldGroup>
          {error ? (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          ) : null}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              취소
            </Button>
            <Button type="submit" disabled={form.formState.isSubmitting || !!mountMsg}>
              {form.formState.isSubmitting ? <Spinner data-icon="inline-start" /> : null}
              저장하고 인증하기
            </Button>
          </DialogFooter>
        </form>
  )
}

/** Shows the authorization link and finishes the connection for the chosen mode. */
function AuthDialog({ start, onClose }: { start: AuthStart | null; onClose: () => void }) {
  return (
    <Dialog open={!!start} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">{start ? <AuthStep key={start.authorize_url} start={start} onClose={onClose} /> : null}</DialogContent>
    </Dialog>
  )
}

function AuthStep({ start, onClose }: { start: AuthStart; onClose: () => void }) {
  const id = start.storage.id
  const mode = start.storage.auth_mode
  const since = start.storage.authorized_at
  const status = useQuery({
    queryKey: ["storage", id, start.authorize_url],
    queryFn: () => api.storage(id),
    refetchInterval: (q) => (q.state.data?.status === "active" && q.state.data.authorized_at > since ? false : 2000),
  })
  const connected = !!status.data && status.data.status === "active" && !!status.data.account_email && status.data.authorized_at > since
  const [code, setCode] = useState("")
  const [url, setUrl] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (connected) refreshAll()
  }, [connected])

  const finish = async (fn: () => Promise<Storage>) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
      await status.refetch()
      refreshAll()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
        <DialogHeader>
          <DialogTitle>Dropbox 승인: {start.storage.mount_path}</DialogTitle>
          <DialogDescription>새 탭에서 Dropbox에 로그인하고 '허용'을 누르세요. 승인 링크는 10분 동안 쓸 수 있습니다.</DialogDescription>
        </DialogHeader>
        {connected ? (
          <Alert>
            <CheckCircle2Icon />
            <AlertTitle>연결되었습니다</AlertTitle>
            <AlertDescription>
              {status.data?.account_email} 계정이 {status.data?.mount_path}에 연결되었습니다.
            </AlertDescription>
          </Alert>
        ) : (
          <div className="flex flex-col gap-4">
            <Button nativeButton={false} render={<a href={start.authorize_url} target="_blank" rel="noreferrer" />}>
              <ExternalLinkIcon data-icon="inline-start" />
              Dropbox에서 승인하기
            </Button>
            {mode === "code" ? (
              <Field data-invalid={error ? true : undefined}>
                <FieldLabel htmlFor="auth-code">Dropbox가 보여준 코드</FieldLabel>
                <InputGroup>
                  <InputGroupAddon>
                    <KeyRoundIcon />
                  </InputGroupAddon>
                  <InputGroupInput id="auth-code" value={code} onChange={(e) => setCode(e.target.value)} aria-invalid={!!error} />
                  <InputGroupAddon align="inline-end">
                    <InputGroupButton variant="default" disabled={busy || !code.trim()} onClick={() => finish(() => api.submitCode(id, code))}>
                      {busy ? <Spinner data-icon="inline-start" /> : null}
                      연결
                    </InputGroupButton>
                  </InputGroupAddon>
                </InputGroup>
                <FieldError errors={error ? [{ message: error }] : undefined} />
              </Field>
            ) : (
              <>
                <p className="flex items-center gap-2 text-sm text-muted-foreground" aria-live="polite">
                  <Spinner /> 승인을 기다리는 중입니다. 승인하면 자동으로 연결됩니다.
                </p>
                {mode === "localhost" ? (
                  <Field data-invalid={error ? true : undefined}>
                    <FieldLabel htmlFor="auth-url">서버 IP로 접속했다면: 연결 오류 페이지의 주소</FieldLabel>
                    <InputGroup>
                      <InputGroupInput id="auth-url" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="http://localhost:…/api/oauth/callback?code=…" aria-invalid={!!error} />
                      <InputGroupAddon align="inline-end">
                        <InputGroupButton variant="default" disabled={busy || !url.trim()} onClick={() => finish(() => api.pasteUrl(url))}>
                          {busy ? <Spinner data-icon="inline-start" /> : null}
                          연결
                        </InputGroupButton>
                      </InputGroupAddon>
                    </InputGroup>
                    <FieldError errors={error ? [{ message: error }] : undefined} />
                  </Field>
                ) : null}
              </>
            )}
          </div>
        )}
        <DialogFooter>
          <Button variant={connected ? "default" : "outline"} onClick={onClose}>
            {connected ? "완료" : "나중에 하기"}
          </Button>
        </DialogFooter>
    </>
  )
}

function ReauthDialog({ storage, hasPublic, onClose, onStarted }: { storage: Storage | null; hasPublic: boolean; onClose: () => void; onStarted: (a: AuthStart) => void }) {
  return (
    <Dialog open={!!storage} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>{storage ? <ReauthForm key={storage.id} storage={storage} hasPublic={hasPublic} onClose={onClose} onStarted={onStarted} /> : null}</DialogContent>
    </Dialog>
  )
}

function ReauthForm({ storage, hasPublic, onClose, onStarted }: { storage: Storage; hasPublic: boolean; onClose: () => void; onStarted: (a: AuthStart) => void }) {
  const [mode, setMode] = useState<string>(storage.auth_mode === "public" && !hasPublic ? "localhost" : storage.auth_mode)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const run = async () => {
    setBusy(true)
    try {
      onStarted(await api.reauth(storage.id, mode))
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <>
      <DialogHeader>
        <DialogTitle>{storage.mount_path} 다시 인증</DialogTitle>
        <DialogDescription>새 승인 링크를 만듭니다. 이전 링크는 더 이상 쓸 수 없습니다.</DialogDescription>
      </DialogHeader>
      <ToggleGroup value={[mode]} onValueChange={(v) => v[0] && setMode(v[0] as string)} variant="outline" spacing={0} aria-label="인증 방식">
        <ToggleGroupItem value="localhost">localhost</ToggleGroupItem>
        <ToggleGroupItem value="public" disabled={!hasPublic}>
          public_url
        </ToggleGroupItem>
        <ToggleGroupItem value="code">코드 붙여넣기</ToggleGroupItem>
      </ToggleGroup>
      {error ? <p className="text-sm text-destructive">{error}</p> : null}
      <DialogFooter>
        <Button variant="outline" onClick={onClose}>
          취소
        </Button>
        <Button onClick={run} disabled={busy}>
          {busy ? <Spinner data-icon="inline-start" /> : null}
          승인 링크 만들기
        </Button>
      </DialogFooter>
    </>
  )
}

function EditStorageDialog({ storage, onClose }: { storage: Storage | null; onClose: () => void }) {
  return (
    <Dialog open={!!storage} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">{storage ? <EditStorageForm key={storage.id} storage={storage} onClose={onClose} /> : null}</DialogContent>
    </Dialog>
  )
}

function EditStorageForm({ storage, onClose }: { storage: Storage; onClose: () => void }) {
  const form = useForm<EditValues>({
    resolver: zodResolver(baseSchema),
    defaultValues: { name: storage.name, mount_path: storage.mount_path, root_path: storage.root_path, cache_ttl_min: storage.cache_ttl_min },
  })
  const [error, setError] = useState<string | null>(null)
  const mount = useWatch({ control: form.control, name: "mount_path" })
  const mountMsg = useMountCheck(mount ?? "", storage.id)
  const submit = form.handleSubmit(async (v) => {
    try {
      await api.updateStorage(storage.id, v)
      refreshAll()
      queryClient.invalidateQueries({ queryKey: ["list"] })
      toast.add({ title: "저장소를 고쳤습니다", type: "success", timeout: 2500 })
      onClose()
    } catch (e) {
      if (e instanceof ApiError && e.field && e.field in v) form.setError(e.field as keyof EditValues, { message: e.message })
      else setError((e as Error).message)
    }
  })
  const e = form.formState.errors
  const mountError = e.mount_path ?? (mountMsg ? { message: mountMsg } : undefined)
  return (
        <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>저장소 편집</DialogTitle>
            <DialogDescription>{storage.account_email || storage.app_key}</DialogDescription>
          </DialogHeader>
          <FieldGroup>
            <Field data-invalid={mountError ? true : undefined}>
              <FieldLabel htmlFor="e-mount">마운트 경로</FieldLabel>
              <Input id="e-mount" aria-invalid={!!mountError} {...form.register("mount_path")} />
              <FieldError errors={[mountError]} />
            </Field>
            <Field data-invalid={e.root_path ? true : undefined}>
              <FieldLabel htmlFor="e-root">Dropbox 원본 경로</FieldLabel>
              <Input id="e-root" aria-invalid={!!e.root_path} {...form.register("root_path")} />
              <FieldError errors={[e.root_path]} />
            </Field>
            <Field>
              <FieldLabel htmlFor="e-name">이름</FieldLabel>
              <Input id="e-name" {...form.register("name")} />
            </Field>
            <Field data-invalid={e.cache_ttl_min ? true : undefined}>
              <FieldLabel htmlFor="e-ttl">캐시 만료 시간 (분)</FieldLabel>
              <Input id="e-ttl" type="number" className="w-32" aria-invalid={!!e.cache_ttl_min} {...form.register("cache_ttl_min", { valueAsNumber: true })} />
              <FieldError errors={[e.cache_ttl_min]} />
            </Field>
          </FieldGroup>
          {error ? <p className="text-sm text-destructive">{error}</p> : null}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              취소
            </Button>
            <Button type="submit" disabled={form.formState.isSubmitting || !!mountMsg}>
              {form.formState.isSubmitting ? <Spinner data-icon="inline-start" /> : null}
              저장
            </Button>
          </DialogFooter>
        </form>
  )
}

function DeleteStorageDialog({ storage, onClose }: { storage: Storage | null; onClose: () => void }) {
  const [busy, setBusy] = useState(false)
  const label = useMemo(() => storage?.mount_path ?? "", [storage])
  const run = async () => {
    if (!storage) return
    setBusy(true)
    try {
      await api.deleteStorage(storage.id)
      refreshAll()
      toast.add({ title: `${label} 저장소를 삭제했습니다`, type: "success", timeout: 2500 })
      onClose()
    } catch (e) {
      toast.add({ title: "삭제하지 못했습니다", description: (e as Error).message, type: "error" })
    } finally {
      setBusy(false)
    }
  }
  return (
    <AlertDialog open={!!storage} onOpenChange={(o) => !o && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{label} 저장소를 삭제할까요?</AlertDialogTitle>
          <AlertDialogDescription>
            이 사이트와 Dropbox 계정의 연결만 끊고 토큰을 폐기합니다. Dropbox 안의 파일은 그대로 남습니다.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>취소</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={run} disabled={busy}>
            {busy ? <Spinner data-icon="inline-start" /> : null}
            저장소 삭제
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
