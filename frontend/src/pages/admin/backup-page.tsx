import { useQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import {
  AlertTriangleIcon,
  DownloadIcon,
  FileCheck2Icon,
  HistoryIcon,
  LockIcon,
  RotateCcwIcon,
  ShieldAlertIcon,
  UploadIcon,
} from "lucide-react"
import { useRef, useState } from "react"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import {
  Field,
  FieldContent,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { toast } from "@/components/ui/toast"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { api, type BackupSnapshot, type RestorePreview, type RestorePreviewStorage } from "@/lib/api"
import { keys, queryClient } from "@/lib/queries"

const MAX_BACKUP_FILE_BYTES = 10 * 1024 * 1024
const MIN_PASSWORD_CHARS = 8
const MAX_PASSWORD_CHARS = 128

const dateTimeSecFmt = new Intl.DateTimeFormat("ko-KR", {
  year: "numeric",
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
})

function formatDateTimeSec(iso: string | undefined): string {
  if (!iso) return "—"
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return dateTimeSecFmt.format(d)
}

function formatSnapshotSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "0.0 KB"
  if (bytes >= 1024 * 1024) {
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
  }
  return `${(bytes / 1024).toFixed(1)} KB`
}

function codePointLength(value: string): number {
  return Array.from(value).length
}

function sanitizeErrorMessage(message: string, secrets: string[]): string {
  let result = message
  for (const s of secrets) {
    if (s && s.length > 0 && result.includes(s)) {
      result = result.split(s).join("[REDACTED]")
    }
  }
  return result
}

function StorageStatusBadge({ status }: { status: string }) {
  if (status === "active") return <Badge variant="secondary">연결됨 (active)</Badge>
  if (status === "error") return <Badge variant="destructive">재인증 필요 (error)</Badge>
  if (status === "pending") return <Badge variant="outline">인증 대기 (pending)</Badge>
  return <Badge variant="outline">{status || "알 수 없음"}</Badge>
}

function EncryptionModeBadge({ mode, encrypted }: { mode: RestorePreview["encryption_mode"]; encrypted: boolean }) {
  if (!encrypted || mode === "none") {
    return <Badge variant="outline">평문 (none)</Badge>
  }
  if (mode === "instance-key") {
    return <Badge variant="secondary">인스턴스 키 암호화 (instance-key)</Badge>
  }
  return <Badge variant="default">비밀번호 암호화 (password)</Badge>
}

export default function BackupPage() {
  const navigate = useNavigate()

  // Section A: Backup Export state
  const [encrypted, setEncrypted] = useState(true)
  const [password, setPassword] = useState("")
  const [passwordConfirm, setPasswordConfirm] = useState("")
  const [confirmUnencrypted, setConfirmUnencrypted] = useState(false)
  const [exportAttempted, setExportAttempted] = useState(false)
  const [exportBusy, setExportBusy] = useState(false)
  const [exportError, setExportError] = useState<string | null>(null)

  // Section B: Restore from File state
  const fileInputRef = useRef<HTMLInputElement>(null)
  const restorePasswordRef = useRef<HTMLInputElement>(null)
  const [selectedFile, setSelectedFile] = useState<File | null>(null)
  const [fileSizeError, setFileSizeError] = useState<string | null>(null)
  const [inspectedMode, setInspectedMode] = useState<"password" | "none" | "instance-key" | null>(null)
  const [restorePassword, setRestorePassword] = useState("")
  const [previewBusy, setPreviewBusy] = useState(false)
  const [restoreError, setRestoreError] = useState<string | null>(null)

  // Section C: Pre-Restore Snapshots state
  const snapshotsQuery = useQuery({
    queryKey: ["admin", "backup-snapshots"],
    queryFn: api.admin.listSnapshots,
  })
  const [snapshotBusyId, setSnapshotBusyId] = useState<string | null>(null)
  const [snapshotError, setSnapshotError] = useState<string | null>(null)

  // Confirmation Dialog state
  const [activePreview, setActivePreview] = useState<RestorePreview | null>(null)
  const [applyBusy, setApplyBusy] = useState(false)
  const [applyError, setApplyError] = useState<string | null>(null)

  const passwordLen = codePointLength(password)
  const passwordTooShort = encrypted && (password.length > 0 || exportAttempted) && passwordLen < MIN_PASSWORD_CHARS
  const passwordTooLong = encrypted && passwordLen > MAX_PASSWORD_CHARS
  const passwordMismatch =
    encrypted && (passwordConfirm.length > 0 || exportAttempted) && password !== passwordConfirm

  const passwordErrorMsg = passwordTooShort
    ? `백업 비밀번호는 ${MIN_PASSWORD_CHARS}자 이상이어야 합니다 (현재 ${passwordLen}자)`
    : passwordTooLong
      ? `백업 비밀번호는 ${MAX_PASSWORD_CHARS}자 이하여야 합니다 (현재 ${passwordLen}자)`
      : null

  const passwordConfirmErrorMsg = passwordMismatch ? "비밀번호와 확인 입력값이 일치하지 않습니다" : null

  const canExport = encrypted
    ? passwordLen >= MIN_PASSWORD_CHARS && passwordLen <= MAX_PASSWORD_CHARS && password === passwordConfirm
    : confirmUnencrypted

  const handleEncryptedChange = (nextEncrypted: boolean) => {
    setEncrypted(nextEncrypted)
    setExportError(null)
    setExportAttempted(false)
    if (nextEncrypted) {
      setConfirmUnencrypted(false)
    } else {
      setPassword("")
      setPasswordConfirm("")
    }
  }

  const handleExportSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setExportAttempted(true)
    setExportError(null)

    if (!canExport) return

    setExportBusy(true)
    const currentPassword = password
    try {
      const { blob, filename } = await api.admin.exportBackup(
        encrypted
          ? { encrypted: true, password: currentPassword }
          : { encrypted: false, confirm_unencrypted: confirmUnencrypted },
      )
      const objectUrl = URL.createObjectURL(blob)
      try {
        const a = document.createElement("a")
        a.href = objectUrl
        a.download = filename
        document.body.appendChild(a)
        a.click()
        a.remove()
      } finally {
        URL.revokeObjectURL(objectUrl)
      }
      setPassword("")
      setPasswordConfirm("")
      setConfirmUnencrypted(false)
      setExportAttempted(false)
      toast.add({
        title: "백업 파일을 다운로드했습니다",
        description: filename,
        type: "success",
        timeout: 3000,
      })
    } catch (err) {
      setExportError(sanitizeErrorMessage((err as Error).message, [currentPassword]))
    } finally {
      setExportBusy(false)
    }
  }

  const handleFileChange = async (file: File | null) => {
    setRestoreError(null)
    setInspectedMode(null)

    if (!file) {
      setSelectedFile(null)
      setFileSizeError(null)
      return
    }

    setSelectedFile(file)
    if (file.size > MAX_BACKUP_FILE_BYTES) {
      setFileSizeError("백업 파일 크기는 10MB를 초과할 수 없습니다")
      return
    }

    setFileSizeError(null)

    try {
      const text = await file.text()
      const parsed = JSON.parse(text) as {
        encrypted?: unknown
        encryption_mode?: unknown
      }
      if (parsed && typeof parsed === "object") {
        if (parsed.encrypted === true) {
          const mode = parsed.encryption_mode === "instance-key" ? "instance-key" : "password"
          setInspectedMode(mode)
          if (mode === "password") {
            setTimeout(() => restorePasswordRef.current?.focus(), 0)
          }
        } else if (parsed.encrypted === false) {
          setInspectedMode("none")
        }
      }
    } catch {
      // Leave inspectedMode as null so server-side parser handles validation.
    }
  }

  const requiresRestorePassword = inspectedMode === "password"
  const canPreviewRestore =
    Boolean(selectedFile) &&
    !fileSizeError &&
    !previewBusy &&
    (!requiresRestorePassword || restorePassword.length > 0)

  const handlePreviewRestoreSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!selectedFile || fileSizeError) return
    if (requiresRestorePassword && restorePassword.length === 0) return

    setPreviewBusy(true)
    setRestoreError(null)
    setApplyError(null)
    const currentPassword = restorePassword
    try {
      const preview = await api.admin.previewRestore(
        selectedFile,
        currentPassword.length > 0 ? currentPassword : undefined,
      )
      setActivePreview(preview)
    } catch (err) {
      setRestoreError(sanitizeErrorMessage((err as Error).message, [currentPassword]))
    } finally {
      setPreviewBusy(false)
    }
  }

  const handlePreviewSnapshot = async (snapshotId: string) => {
    setSnapshotBusyId(snapshotId)
    setSnapshotError(null)
    setApplyError(null)
    try {
      const preview = await api.admin.previewSnapshot(snapshotId)
      setActivePreview(preview)
    } catch (err) {
      setSnapshotError((err as Error).message)
    } finally {
      setSnapshotBusyId(null)
    }
  }

  const handleApplyRestore = async () => {
    if (!activePreview) return
    setApplyBusy(true)
    setApplyError(null)
    try {
      const res = await api.admin.applyRestore(activePreview.preview_id)
      setRestorePassword("")
      setActivePreview(null)
      if (res.failed_storages && res.failed_storages.length > 0) {
        toast.add({
          title: "일부 저장소 클라이언트를 초기화하지 못했습니다",
          description: res.failed_storages.join(", "),
          type: "warning",
          timeout: 5000,
        })
      }
      queryClient.clear()
      queryClient.setQueryData(keys.me, { authenticated: false })
      await navigate({ to: "/login", search: { restored: "1", next: undefined }, replace: true })
    } catch (err) {
      setApplyError(sanitizeErrorMessage((err as Error).message, [restorePassword]))
    } finally {
      setApplyBusy(false)
    }
  }

  const sortedSnapshots: BackupSnapshot[] = snapshotsQuery.data?.snapshots
    ? [...snapshotsQuery.data.snapshots]
        .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())
        .slice(0, 5)
    : []

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <div>
        <h1 className="text-xl font-semibold">백업 및 복원</h1>
        <p className="text-sm text-muted-foreground">
          관리자 계정, 시스템 설정, Dropbox 저장소 연결 정보를 백업 파일(.json)로 내보내거나 이전 시점으로 복원합니다.
        </p>
      </div>

      {/* Section A: 백업 내보내기 */}
      <Card>
        <form onSubmit={handleExportSubmit} noValidate>
          <CardHeader>
            <CardTitle>백업 내보내기</CardTitle>
            <CardDescription>
              관리자 계정, 시스템 설정, 등록된 모든 Dropbox OAuth 인증 정보(app_key, app_secret, refresh_token)를 이동 가능한{" "}
              <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">.json</code> 파일로 내려받습니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-5 py-4">
            <FieldGroup>
              <Field orientation="horizontal">
                <FieldContent>
                  <FieldTitle>비밀번호로 백업 암호화</FieldTitle>
                  <FieldDescription>
                    백업 파일이 외부에 노출되더라도 비밀번호 없이는 Dropbox 토큰과 설정을 읽을 수 없도록 보호합니다.
                  </FieldDescription>
                </FieldContent>
                <div className="flex items-center gap-3">
                  <ToggleGroup
                    value={[encrypted ? "encrypted" : "unencrypted"]}
                    onValueChange={(v) => {
                      if (v[0] === "encrypted") handleEncryptedChange(true)
                      else if (v[0] === "unencrypted") handleEncryptedChange(false)
                    }}
                    variant="outline"
                    size="sm"
                    spacing={0}
                    aria-label="백업 암호화 방식"
                  >
                    <ToggleGroupItem value="encrypted">암호화 사용</ToggleGroupItem>
                    <ToggleGroupItem value="unencrypted">암호화 미사용</ToggleGroupItem>
                  </ToggleGroup>
                  <Switch
                    id="backup-encrypted"
                    checked={encrypted}
                    onCheckedChange={handleEncryptedChange}
                    aria-label="암호화 보호 스위치"
                  />
                </div>
              </Field>

              {encrypted ? (
                <>
                  <Field data-invalid={passwordErrorMsg ? true : undefined}>
                    <FieldLabel htmlFor="backup-password">새 백업 비밀번호</FieldLabel>
                    <Input
                      id="backup-password"
                      type="password"
                      autoComplete="new-password"
                      placeholder="8자 이상 입력"
                      value={password}
                      onChange={(e) => {
                        setPassword(e.target.value)
                        setExportError(null)
                      }}
                      aria-invalid={!!passwordErrorMsg}
                    />
                    <FieldDescription>복원할 때 동일한 비밀번호가 필요합니다 (8~128자).</FieldDescription>
                    <FieldError errors={passwordErrorMsg ? [{ message: passwordErrorMsg }] : undefined} />
                  </Field>

                  <Field data-invalid={passwordConfirmErrorMsg ? true : undefined}>
                    <FieldLabel htmlFor="backup-password-confirm">백업 비밀번호 확인</FieldLabel>
                    <Input
                      id="backup-password-confirm"
                      type="password"
                      autoComplete="new-password"
                      placeholder="비밀번호 다시 입력"
                      value={passwordConfirm}
                      onChange={(e) => {
                        setPasswordConfirm(e.target.value)
                        setExportError(null)
                      }}
                      aria-invalid={!!passwordConfirmErrorMsg}
                    />
                    <FieldError errors={passwordConfirmErrorMsg ? [{ message: passwordConfirmErrorMsg }] : undefined} />
                  </Field>
                </>
              ) : (
                <div className="flex flex-col gap-4">
                  <Alert variant="destructive">
                    <ShieldAlertIcon />
                    <AlertTitle>평문 백업 주의</AlertTitle>
                    <AlertDescription>
                      암호화를 끄면 다운로드되는 백업 파일 안에 모든 저장소의 Dropbox{" "}
                      <code className="font-mono text-xs">app_key</code>,{" "}
                      <code className="font-mono text-xs">app_secret</code>,{" "}
                      <code className="font-mono text-xs">refresh_token</code> 인증 정보가 암호화되지 않은 평문으로 저장됩니다.
                    </AlertDescription>
                  </Alert>

                  <Field orientation="horizontal">
                    <Checkbox
                      id="confirm-unencrypted"
                      checked={confirmUnencrypted}
                      onCheckedChange={(checked) => {
                        setConfirmUnencrypted(Boolean(checked))
                        setExportError(null)
                      }}
                    />
                    <FieldLabel htmlFor="confirm-unencrypted" className="cursor-pointer font-normal">
                      암호화되지 않은 백업 파일에 Dropbox 인증 정보가 평문으로 포함됨을 이해했습니다
                    </FieldLabel>
                  </Field>
                </div>
              )}
            </FieldGroup>

            {exportError ? (
              <Alert variant="destructive">
                <AlertTriangleIcon />
                <AlertDescription>{exportError}</AlertDescription>
              </Alert>
            ) : null}
          </CardContent>
          <CardFooter className="justify-end">
            <Button type="submit" disabled={!canExport || exportBusy}>
              {exportBusy ? <Spinner data-icon="inline-start" /> : <DownloadIcon data-icon="inline-start" />}
              백업 파일 다운로드
            </Button>
          </CardFooter>
        </form>
      </Card>

      {/* Section B: 백업 파일에서 복원 */}
      <Card>
        <form onSubmit={handlePreviewRestoreSubmit} noValidate>
          <CardHeader>
            <CardTitle>백업 파일에서 복원</CardTitle>
            <CardDescription>
              백업 파일(<code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">.json</code>)을 올려 무결성을 검증하고, 복원될 항목을 미리 확인한 뒤 적용합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-5 py-4">
            <FieldGroup>
              <Field data-invalid={fileSizeError ? true : undefined}>
                <FieldLabel htmlFor="restore-file">백업 파일 선택</FieldLabel>
                <Input
                  ref={fileInputRef}
                  id="restore-file"
                  type="file"
                  accept=".json,application/json"
                  aria-invalid={!!fileSizeError}
                  onChange={(e) => {
                    const f = e.target.files?.[0] ?? null
                    void handleFileChange(f)
                  }}
                />
                <FieldDescription>최대 10MB 이하의 dvfs-backup JSON 파일만 업로드할 수 있습니다.</FieldDescription>
                {selectedFile && !fileSizeError ? (
                  <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                    <UploadIcon className="size-3.5" aria-hidden />
                    <span className="font-medium text-foreground">{selectedFile.name}</span>
                    <span>({formatSnapshotSize(selectedFile.size)})</span>
                    {inspectedMode === "password" ? (
                      <Badge variant="default">
                        <LockIcon data-icon="inline-start" />
                        암호화됨
                      </Badge>
                    ) : inspectedMode === "none" ? (
                      <Badge variant="outline">평문 백업</Badge>
                    ) : inspectedMode === "instance-key" ? (
                      <Badge variant="secondary">인스턴스 키 백업</Badge>
                    ) : null}
                  </div>
                ) : null}
                <FieldError errors={fileSizeError ? [{ message: fileSizeError }] : undefined} />
              </Field>

              <Field>
                <FieldLabel htmlFor="restore-password">복호화 비밀번호</FieldLabel>
                <Input
                  ref={restorePasswordRef}
                  id="restore-password"
                  type="password"
                  autoComplete="current-password"
                  placeholder={
                    inspectedMode === "none"
                      ? "평문 백업 파일입니다 (비밀번호 불필요)"
                      : "암호화된 백업 파일의 비밀번호 입력"
                  }
                  value={restorePassword}
                  onChange={(e) => {
                    setRestorePassword(e.target.value)
                    setRestoreError(null)
                  }}
                />
                <FieldDescription>
                  {inspectedMode === "password"
                    ? "선택한 파일은 비밀번호로 암호화되어 있습니다. 복호화 비밀번호를 입력해야 검증할 수 있습니다."
                    : inspectedMode === "none"
                      ? "선택한 파일은 암호화되지 않은 백업입니다. 비밀번호를 비워 두세요."
                      : "비밀번호로 암호화된 백업 파일을 복원할 때 입력합니다."}
                </FieldDescription>
              </Field>
            </FieldGroup>

            {restoreError ? (
              <Alert variant="destructive">
                <AlertTriangleIcon />
                <AlertDescription>{restoreError}</AlertDescription>
              </Alert>
            ) : null}
          </CardContent>
          <CardFooter className="justify-end">
            <Button type="submit" disabled={!canPreviewRestore}>
              {previewBusy ? <Spinner data-icon="inline-start" /> : <FileCheck2Icon data-icon="inline-start" />}
              백업 검증 및 미리보기
            </Button>
          </CardFooter>
        </form>
      </Card>

      {/* Section C: 복원 전 자동 스냅샷 */}
      <Card>
        <CardHeader>
          <CardTitle>복원 전 자동 스냅샷</CardTitle>
          <CardDescription>
            복원을 실행할 때마다 직전 상태가 서버에 자동 보관됩니다(최신순 최대 5개). 잘못 복원한 경우 비밀번호 없이 바로 이전 상태로 되돌릴 수 있습니다.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {snapshotError ? (
            <Alert variant="destructive">
              <AlertTriangleIcon />
              <AlertDescription>{snapshotError}</AlertDescription>
            </Alert>
          ) : null}

          {snapshotsQuery.isPending ? (
            <div className="flex items-center gap-2 py-4 text-sm text-muted-foreground">
              <Spinner />
              <span>자동 스냅샷 목록을 불러오는 중입니다…</span>
            </div>
          ) : snapshotsQuery.isError ? (
            <Alert variant="destructive">
              <AlertTriangleIcon />
              <AlertDescription>{(snapshotsQuery.error as Error).message}</AlertDescription>
            </Alert>
          ) : sortedSnapshots.length === 0 ? (
            <Empty className="border">
              <EmptyHeader>
                <EmptyMedia variant="icon">
                  <HistoryIcon />
                </EmptyMedia>
                <EmptyTitle>보관된 자동 스냅샷 없음</EmptyTitle>
                <EmptyDescription>
                  아직 생성된 복원 전 자동 스냅샷이 없습니다. 복원을 실행하면 직전 상태가 자동으로 보관됩니다.
                </EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : (
            <div className="divide-y rounded-lg border" role="list" aria-label="복원 전 자동 스냅샷 목록">
              {sortedSnapshots.map((snap) => {
                const isThisBusy = snapshotBusyId === snap.id
                return (
                  <div
                    key={snap.id}
                    role="listitem"
                    className="flex flex-wrap items-center justify-between gap-3 px-4 py-3"
                  >
                    <div className="flex min-w-0 flex-col gap-1">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="font-medium tabular-nums">{formatDateTimeSec(snap.created_at)}</span>
                        <Badge variant="secondary">v{snap.app_version}</Badge>
                        <Badge variant="outline">저장소 {snap.storage_count}개</Badge>
                      </div>
                      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground tabular-nums">
                        <span className="font-mono break-all">{snap.id}</span>
                        <span>·</span>
                        <span>{formatSnapshotSize(snap.size)}</span>
                      </div>
                    </div>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      disabled={snapshotBusyId !== null || previewBusy}
                      onClick={() => void handlePreviewSnapshot(snap.id)}
                    >
                      {isThisBusy ? (
                        <Spinner data-icon="inline-start" />
                      ) : (
                        <RotateCcwIcon data-icon="inline-start" />
                      )}
                      이 스냅샷으로 복원
                    </Button>
                  </div>
                )
              })}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Restore Confirmation Dialog */}
      <RestoreConfirmDialog
        preview={activePreview}
        busy={applyBusy}
        error={applyError}
        onCancel={() => {
          if (!applyBusy) {
            setActivePreview(null)
            setApplyError(null)
          }
        }}
        onConfirm={() => void handleApplyRestore()}
      />
    </div>
  )
}

function RestoreConfirmDialog({
  preview,
  busy,
  error,
  onCancel,
  onConfirm,
}: {
  preview: RestorePreview | null
  busy: boolean
  error: string | null
  onCancel: () => void
  onConfirm: () => void
}) {
  return (
    <Dialog open={preview !== null} onOpenChange={(open) => !open && onCancel()}>
      <DialogContent className="sm:max-w-lg" showCloseButton={!busy}>
        {preview ? (
          <div className="flex flex-col gap-4">
            <DialogHeader>
              <DialogTitle>백업 복원 확인</DialogTitle>
              <DialogDescription>
                현재 관리자 계정, 설정, 저장소 목록이 백업 내용으로 바뀝니다.
              </DialogDescription>
            </DialogHeader>

            <div className="rounded-lg border bg-muted/30 p-3">
              <dl className="grid grid-cols-[7.5rem_1fr] gap-x-3 gap-y-2 text-sm">
                <dt className="text-muted-foreground">백업 생성 시각</dt>
                <dd className="font-medium tabular-nums">{formatDateTimeSec(preview.created_at)}</dd>

                <dt className="text-muted-foreground">앱 / 형식 버전</dt>
                <dd className="tabular-nums">
                  {preview.app_version} <span className="text-xs text-muted-foreground">(형식 v{preview.format_version})</span>
                </dd>

                <dt className="text-muted-foreground">암호화 방식</dt>
                <dd>
                  <EncryptionModeBadge mode={preview.encryption_mode} encrypted={preview.encrypted} />
                </dd>

                <dt className="text-muted-foreground">복원될 관리자</dt>
                <dd className="font-medium break-all">{preview.admin_username}</dd>

                <dt className="text-muted-foreground">시스템 설정</dt>
                <dd className="tabular-nums">{preview.settings_count}개 항목</dd>
              </dl>
            </div>

            <div className="flex flex-col gap-2">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium">스토리지 마운트 목록</span>
                <Badge variant="secondary">{preview.storages.length}개</Badge>
              </div>
              {preview.storages.length === 0 ? (
                <p className="rounded-lg border border-dashed px-3 py-3 text-xs text-muted-foreground">
                  백업에 등록된 저장소가 없습니다. 복원하면 현재 등록된 저장소가 모두 제거됩니다.
                </p>
              ) : (
                <div
                  className="max-h-44 divide-y overflow-y-auto rounded-lg border"
                  role="list"
                  aria-label="복원될 스토리지 목록"
                >
                  {preview.storages.map((s: RestorePreviewStorage) => (
                    <div
                      key={s.mount_path}
                      role="listitem"
                      className="flex flex-wrap items-center justify-between gap-2 px-3 py-2 text-sm"
                    >
                      <div className="flex min-w-0 items-center gap-2">
                        <span className="font-medium break-all">{s.mount_path}</span>
                        {s.name ? <span className="truncate text-xs text-muted-foreground">({s.name})</span> : null}
                      </div>
                      <StorageStatusBadge status={s.status} />
                    </div>
                  ))}
                </div>
              )}
            </div>

            <Alert variant="destructive">
              <AlertTriangleIcon />
              <AlertTitle>복원 실행 안내</AlertTitle>
              <AlertDescription>
                <ul className="list-disc pl-4">
                  <li>현재 상태를 자동 스냅샷으로 먼저 보관한 뒤, 관리자 계정·설정·스토리지 구성을 백업 시점으로 덮어씁니다.</li>
                  <li>현재 로그인 세션을 포함한 모든 세션이 즉시 만료되어 로그인 화면으로 이동합니다.</li>
                </ul>
              </AlertDescription>
            </Alert>

            {error ? (
              <Alert variant="destructive">
                <AlertTriangleIcon />
                <AlertDescription>{error}</AlertDescription>
              </Alert>
            ) : null}

            <DialogFooter>
              <Button type="button" variant="outline" onClick={onCancel} disabled={busy}>
                취소
              </Button>
              <Button type="button" variant="destructive" onClick={onConfirm} disabled={busy}>
                {busy ? <Spinner data-icon="inline-start" /> : <RotateCcwIcon data-icon="inline-start" />}
                복원 실행
              </Button>
            </DialogFooter>
          </div>
        ) : null}
      </DialogContent>
    </Dialog>
  )
}
