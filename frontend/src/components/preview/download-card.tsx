import { Link } from "@tanstack/react-router"
import { DownloadIcon, EyeIcon, LinkIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card"
import { EntryIcon } from "@/components/browser/entry-icon"
import { copyLink, downloadEntries } from "@/components/browser/actions"
import type { Entry } from "@/lib/api"
import { extOf, formatBytes, formatDate } from "@/lib/format"
import { toSplat } from "@/lib/paths"
import { useConfig } from "@/lib/queries"

/** Download tab content: name and extension, size, date, copy-link and download buttons. */
export function DownloadCard({
  entry,
  note,
  showPreviewLink,
}: {
  entry: Entry
  note?: string
  showPreviewLink?: boolean
}) {
  const cfg = useConfig()
  const ext = extOf(entry.name)
  const base = ext ? entry.name.slice(0, entry.name.length - ext.length - 1) : entry.name
  return (
    <Card className="max-w-xl">
      <CardHeader>
        <CardTitle className="flex items-start gap-2 break-all">
          <EntryIcon entry={entry} className="mt-0.5 size-5" />
          <span>
            {base}
            {ext ? <span className="text-muted-foreground">.{ext}</span> : null}
          </span>
        </CardTitle>
        {note ? <CardDescription>{note}</CardDescription> : null}
      </CardHeader>
      <CardContent>
        <dl className="grid grid-cols-[6rem_1fr] gap-y-1.5 text-sm">
          <dt className="text-muted-foreground">형식</dt>
          <dd>{ext ? ext.toUpperCase() : "없음"}</dd>
          <dt className="text-muted-foreground">용량</dt>
          <dd className="tabular-nums">
            {formatBytes(entry.size)}
            {entry.size != null && entry.size >= 1024 ? <span className="text-muted-foreground"> ({entry.size.toLocaleString()} 바이트)</span> : null}
          </dd>
          <dt className="text-muted-foreground">수정 시간</dt>
          <dd className="tabular-nums">{formatDate(entry.modified)}</dd>
          <dt className="text-muted-foreground">경로</dt>
          <dd className="break-all">{entry.path}</dd>
        </dl>
      </CardContent>
      <CardFooter className="justify-end gap-2">
        {showPreviewLink ? (
          <Button
            variant="outline"
            nativeButton={false}
            render={<Link to="/$" params={{ _splat: toSplat(entry.path) }} />}
          >
            <EyeIcon data-icon="inline-start" />
            미리보기 탭
          </Button>
        ) : null}
        <Button variant="outline" onClick={() => copyLink(entry.path, cfg.data?.public_url ?? "")}>
          <LinkIcon data-icon="inline-start" />
          링크 복사
        </Button>
        <Button onClick={() => downloadEntries([entry])}>
          <DownloadIcon data-icon="inline-start" />
          다운로드
        </Button>
      </CardFooter>
    </Card>
  )
}
