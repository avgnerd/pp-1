import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { ArrowUpIcon, FolderIcon } from "lucide-react"
import { useMemo, useState } from "react"
import { useForm } from "react-hook-form"
import { z } from "zod"

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
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import { api, type Entry, type OpResult } from "@/lib/api"
import { baseName, joinPath, parentPath } from "@/lib/paths"
import { afterChange, useMounts } from "@/lib/queries"
import { useSelection } from "@/stores/ui"

import { useActionDialog } from "./actions"

function reportResult(verb: string, r: OpResult) {
  const fails = r.failures ?? []
  if (fails.length === 0) {
    toast.add({ title: `${r.done}개 항목을 ${verb}했습니다`, type: "success", timeout: 3000 })
    return
  }
  toast.add({
    title: `${r.done}개 ${verb}, ${fails.length}개 실패`,
    description: fails
      .slice(0, 3)
      .map((f) => `${baseName(f.path)}: ${f.error}`)
      .join("\n"),
    type: "error",
  })
}

export function ActionDialogs() {
  const { kind, entries, close } = useActionDialog()
  return (
    <>
      <RenameDialog open={kind === "rename"} entry={entries[0]} onClose={close} />
      <DestinationDialog open={kind === "move" || kind === "copy"} mode={kind === "copy" ? "copy" : "move"} entries={entries} onClose={close} />
      <DeleteDialog open={kind === "delete"} entries={entries} onClose={close} />
    </>
  )
}

const nameSchema = z.object({
  name: z
    .string()
    .min(1, "이름을 입력하세요")
    .max(255, "이름이 너무 깁니다")
    .refine((v) => !/[\\/]/.test(v), "이름에 / 또는 \\를 쓸 수 없습니다")
    .refine((v) => v.trim() === v, "이름 앞뒤에 공백을 둘 수 없습니다")
    .refine((v) => v !== "." && v !== "..", "쓸 수 없는 이름입니다"),
})

function RenameDialog({ open, entry, onClose }: { open: boolean; entry?: Entry; onClose: () => void }) {
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>{open && entry ? <RenameForm key={entry.path} entry={entry} onClose={onClose} /> : null}</DialogContent>
    </Dialog>
  )
}

function RenameForm({ entry, onClose }: { entry: Entry; onClose: () => void }) {
  const form = useForm<{ name: string }>({ resolver: zodResolver(nameSchema), defaultValues: { name: entry.name } })
  const [serverError, setServerError] = useState<string | null>(null)

  const submit = form.handleSubmit(async ({ name }) => {
    if (name === entry.name) return onClose()
    try {
      await api.rename(entry.path, name)
      afterChange([entry.path, joinPath(parentPath(entry.path), name)])
      toast.add({ title: "이름을 바꿨습니다", description: name, type: "success", timeout: 3000 })
      onClose()
    } catch (e) {
      setServerError((e as Error).message)
    }
  })

  const err = form.formState.errors.name
  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle>이름 바꾸기</DialogTitle>
        <DialogDescription className="break-all">{entry.path}</DialogDescription>
      </DialogHeader>
      <FieldGroup>
        <Field data-invalid={err || serverError ? true : undefined}>
          <FieldLabel htmlFor="rename-name">새 이름</FieldLabel>
          <Input
            id="rename-name"
            autoFocus
            aria-invalid={!!err || !!serverError}
            {...form.register("name")}
            onFocus={(e) => {
              // Select the base name without the extension.
              const v = e.currentTarget.value
              const dot = entry.type === "file" ? v.lastIndexOf(".") : -1
              e.currentTarget.setSelectionRange(0, dot > 0 ? dot : v.length)
            }}
          />
          <FieldError errors={[err, serverError ? { message: serverError } : undefined]} />
        </Field>
      </FieldGroup>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onClose}>
          취소
        </Button>
        <Button type="submit" disabled={form.formState.isSubmitting}>
          {form.formState.isSubmitting ? <Spinner data-icon="inline-start" /> : null}
          이름 바꾸기
        </Button>
      </DialogFooter>
    </form>
  )
}

function DestinationDialog({ open, mode, entries, onClose }: { open: boolean; mode: "move" | "copy"; entries: Entry[]; onClose: () => void }) {
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">
        {open && entries[0] ? <DestinationPicker key={entries.map((e) => e.path).join("|")} mode={mode} entries={entries} onClose={onClose} /> : null}
      </DialogContent>
    </Dialog>
  )
}

function DestinationPicker({ mode, entries, onClose }: { mode: "move" | "copy"; entries: Entry[]; onClose: () => void }) {
  const mounts = useMounts()
  const mountRoot = useMemo(() => {
    const first = entries[0]
    return mounts.data?.find((m) => first.path === m.mount_path || first.path.startsWith(m.mount_path + "/"))?.mount_path ?? "/"
  }, [entries, mounts.data])
  const [dir, setDir] = useState(() => parentPath(entries[0].path))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const clearSel = useSelection((s) => s.clear)

  const folders = useQuery({
    queryKey: ["picker", dir],
    queryFn: () => api.list(dir, { sort: "name", order: "asc" }),
    select: (r) => r.entries.filter((e) => e.type === "folder" && !entries.some((s) => s.path === e.path)),
  })

  const verb = mode === "move" ? "이동" : "복사"
  const sameDir = mode === "move" && entries.every((e) => parentPath(e.path) === dir)

  const run = async () => {
    setBusy(true)
    setError(null)
    try {
      const paths = entries.map((e) => e.path)
      const r = mode === "move" ? await api.move(paths, dir) : await api.copy(paths, dir)
      afterChange([...paths, dir, ...paths.map((p) => joinPath(dir, baseName(p)))])
      reportResult(verb, r)
      clearSel()
      onClose()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle>
          {entries.length === 1 ? `'${entries[0]?.name}'` : `${entries.length}개 항목`} {verb}
        </DialogTitle>
        <DialogDescription>같은 저장소({mountRoot}) 안에서 대상 폴더를 고르세요.</DialogDescription>
      </DialogHeader>
      <div className="flex items-center gap-2 rounded-md border bg-muted/40 px-2 py-1.5">
        <Button variant="ghost" size="icon-sm" aria-label="상위 폴더" disabled={dir === mountRoot} onClick={() => setDir(parentPath(dir))}>
          <ArrowUpIcon />
        </Button>
        <span className="min-w-0 flex-1 truncate text-sm" title={dir}>
          {dir}
        </span>
      </div>
      <div className="h-64 overflow-y-auto rounded-md border">
        {folders.isPending ? (
          <div className="grid h-full place-items-center text-muted-foreground">
            <Spinner />
          </div>
        ) : folders.isError ? (
          <p className="p-4 text-sm text-destructive">{(folders.error as Error).message}</p>
        ) : folders.data.length === 0 ? (
          <p className="p-4 text-sm text-muted-foreground">하위 폴더가 없습니다. 이 폴더로 {verb}할 수 있습니다.</p>
        ) : (
          <ul className="flex flex-col p-1">
            {folders.data.map((f) => (
              <li key={f.path}>
                <button
                  type="button"
                  onClick={() => setDir(f.path)}
                  className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm hover:bg-accent focus-visible:bg-accent focus-visible:outline-none"
                >
                  <FolderIcon className="size-4 shrink-0 fill-folder/25 text-folder" aria-hidden />
                  <span className="truncate">{f.name}</span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
      {error ? <p className="text-sm text-destructive">{error}</p> : null}
      <DialogFooter>
        <Button variant="outline" onClick={onClose}>
          취소
        </Button>
        <Button onClick={run} disabled={busy || sameDir}>
          {busy ? <Spinner data-icon="inline-start" /> : null}
          {sameDir ? "이미 이 폴더에 있습니다" : `여기로 ${verb}`}
        </Button>
      </DialogFooter>
    </>
  )
}

function DeleteDialog({ open, entries, onClose }: { open: boolean; entries: Entry[]; onClose: () => void }) {
  const [busy, setBusy] = useState(false)
  const clearSel = useSelection((s) => s.clear)
  const run = async () => {
    setBusy(true)
    try {
      const paths = entries.map((e) => e.path)
      const r = await api.remove(paths)
      afterChange(paths)
      reportResult("삭제", r)
      clearSel()
      onClose()
    } catch (e) {
      toast.add({ title: "삭제하지 못했습니다", description: (e as Error).message, type: "error" })
    } finally {
      setBusy(false)
    }
  }
  const folders = entries.filter((e) => e.type === "folder").length
  return (
    <AlertDialog open={open} onOpenChange={(o) => !o && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{entries.length === 1 ? `'${entries[0]?.name}'을(를) 삭제할까요?` : `${entries.length}개 항목을 삭제할까요?`}</AlertDialogTitle>
          <AlertDialogDescription>
            Dropbox 휴지통으로 옮겨집니다.{folders > 0 ? " 폴더 안의 모든 파일도 함께 삭제됩니다." : ""} Dropbox 웹사이트의 삭제된 파일 메뉴에서 복원할 수 있습니다.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>취소</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={run} disabled={busy}>
            {busy ? <Spinner data-icon="inline-start" /> : null}
            삭제
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
