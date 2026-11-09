import { useQuery } from "@tanstack/react-query"
import { useEffect, useState } from "react"
import "highlight.js/styles/github-dark-dimmed.css"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Skeleton } from "@/components/ui/skeleton"
import { api, type Entry } from "@/lib/api"
import { extOf, formatBytes } from "@/lib/format"

const HIGHLIGHT_LIMIT = 256 * 1024

const langByExt: Record<string, string> = {
  js: "javascript", mjs: "javascript", cjs: "javascript", jsx: "javascript", ts: "typescript", tsx: "typescript",
  py: "python", go: "go", rs: "rust", java: "java", kt: "kotlin", c: "c", h: "c", cpp: "cpp", hpp: "cpp", cs: "csharp",
  php: "php", rb: "ruby", sh: "bash", bash: "bash", ps1: "powershell", bat: "dos", cmd: "dos", sql: "sql", lua: "lua",
  pl: "perl", swift: "swift", json: "json", xml: "xml", html: "xml", htm: "xml", vue: "xml", svelte: "xml", yaml: "yaml",
  yml: "yaml", toml: "ini", ini: "ini", cfg: "ini", conf: "ini", properties: "properties", css: "css", scss: "scss",
  md: "markdown", markdown: "markdown", dockerfile: "dockerfile", makefile: "makefile", gradle: "gradle", dart: "dart",
}

export default function TextPreview({ entry }: { entry: Entry }) {
  const q = useQuery({ queryKey: ["text", entry.path, entry.rev], queryFn: () => api.text(entry.path), staleTime: 5 * 60_000 })
  // Highlighted HTML is kept together with the text it was made from, so stale output is never shown.
  const [hl, setHl] = useState<{ src: string; html: string } | null>(null)
  const ext = extOf(entry.name) || entry.name.toLowerCase()
  const lang = langByExt[ext]

  useEffect(() => {
    const text = q.data?.text
    if (text === undefined || !lang || text.length > HIGHLIGHT_LIMIT) return
    let cancelled = false
    import("highlight.js/lib/common").then(({ default: hljs }) => {
      if (cancelled || !hljs.getLanguage(lang)) return
      setHl({ src: text, html: hljs.highlight(text, { language: lang, ignoreIllegals: true }).value })
    })
    return () => {
      cancelled = true
    }
  }, [q.data?.text, lang])
  const html = hl && hl.src === q.data?.text ? hl.html : null

  if (q.isPending) return <Skeleton className="h-96 w-full" />
  if (q.isError) {
    return (
      <Alert variant="destructive">
        <AlertTitle>텍스트를 읽지 못했습니다</AlertTitle>
        <AlertDescription>{(q.error as Error).message}</AlertDescription>
      </Alert>
    )
  }
  return (
    <div className="flex flex-col gap-2">
      {q.data.truncated ? (
        <p className="text-sm text-muted-foreground">
          앞부분 1MB만 표시합니다 (전체 {formatBytes(q.data.size)}). 전체 내용은 다운로드해서 여세요.
        </p>
      ) : null}
      {q.data.encoding === "euc-kr" ? <p className="text-sm text-muted-foreground">EUC-KR(CP949) 인코딩으로 읽었습니다.</p> : null}
      <pre className="max-h-[78svh] overflow-auto rounded-lg border bg-card p-4 text-[0.8125rem] leading-relaxed">
        {html !== null ? (
          <code className="hljs bg-transparent! p-0!" dangerouslySetInnerHTML={{ __html: html }} />
        ) : (
          <code className="whitespace-pre-wrap break-words">{q.data.text}</code>
        )}
      </pre>
    </div>
  )
}
