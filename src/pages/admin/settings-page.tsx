import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { useEffect, useState } from "react"
import { Controller, useForm, type Control } from "react-hook-form"
import { z } from "zod"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card"
import { Field, FieldContent, FieldDescription, FieldError, FieldGroup, FieldLabel, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { toast } from "@/components/ui/toast"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { api, type Settings } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { keys, queryClient } from "@/lib/queries"

const int = (min: number, max: number, label: string) =>
  z.number({ message: `${label}을(를) 숫자로 입력하세요` }).int(`${label}은(는) 정수여야 합니다`).min(min, `${label}은(는) ${min} 이상이어야 합니다`).max(max, `${label}은(는) ${max} 이하여야 합니다`)

const schema = z.object({
  robots_txt: z.string().max(64 * 1024, "robots.txt는 64KB 이하여야 합니다"),
  pagination_type: z.enum(["all", "pages", "more", "auto_more"]),
  per_page: int(5, 1000, "페이지당 항목 수"),
  audio_autoplay: z.boolean(),
  video_autoplay: z.boolean(),
  archive_preview: z.boolean(),
  cache_cleanup_sec: int(10, 86400, "캐시 정리 주기"),
  data_mode: z.enum(["direct", "proxy"]),
  transcode_concurrency: int(0, 16, "동시 변환 수"),
  thumb_cache_mb: int(16, 100000, "썸네일 캐시 용량"),
  folder_size_concurrency: int(1, 32, "폴더 크기 동시 계산 수"),
  search_concurrency: int(1, 64, "검색 동시 요청 수"),
  archive_max_mb: int(1, 100000, "압축 파일 읽기 상한"),
})

export default function SettingsPage() {
  const q = useQuery({ queryKey: ["settings"], queryFn: api.settings })
  const [error, setError] = useState<string | null>(null)
  const form = useForm<Settings>({ resolver: zodResolver(schema) })

  useEffect(() => {
    if (q.data) form.reset(q.data)
  }, [q.data, form])

  const submit = form.handleSubmit(async (v) => {
    setError(null)
    try {
      const saved = await api.updateSettings(v)
      queryClient.setQueryData(["settings"], saved)
      queryClient.invalidateQueries({ queryKey: keys.config })
      queryClient.invalidateQueries({ queryKey: ["list"] })
      form.reset(saved)
      toast.add({ title: "설정을 저장했습니다", type: "success", timeout: 2500 })
    } catch (e) {
      setError((e as Error).message)
    }
  })

  if (q.isPending) return <Spinner />
  if (q.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{(q.error as Error).message}</AlertDescription>
      </Alert>
    )
  }

  const e = form.formState.errors
  const num = (name: keyof Settings) => form.register(name, { valueAsNumber: true })

  return (
    <form onSubmit={submit} className="flex flex-col gap-6" noValidate>
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">설정</h1>
          <p className="text-sm text-muted-foreground">저장하면 서버를 다시 시작하지 않아도 바로 적용됩니다.</p>
        </div>
        <Button type="submit" disabled={form.formState.isSubmitting || !form.formState.isDirty}>
          {form.formState.isSubmitting ? <Spinner data-icon="inline-start" /> : null}
          설정 저장
        </Button>
      </div>
      {error ? (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}

      <Tabs defaultValue="site">
        <TabsList>
          <TabsTrigger value="site">사이트</TabsTrigger>
          <TabsTrigger value="preview">미리보기</TabsTrigger>
          <TabsTrigger value="system">시스템</TabsTrigger>
        </TabsList>

        <TabsContent value="site" className="pt-4">
          <FieldGroup className="max-w-xl">
            <Field data-invalid={e.robots_txt ? true : undefined}>
              <FieldLabel htmlFor="robots">robots.txt</FieldLabel>
              <Textarea id="robots" rows={6} className="font-mono text-sm" aria-invalid={!!e.robots_txt} {...form.register("robots_txt")} />
              <FieldDescription>/robots.txt 주소에 이 내용을 그대로 보여줍니다. 기본값은 모든 검색 로봇을 막습니다.</FieldDescription>
              <FieldError errors={[e.robots_txt]} />
            </Field>
            <Field>
              <FieldLabel>페이지 매기기 유형</FieldLabel>
              <Controller
                control={form.control}
                name="pagination_type"
                render={({ field }) => (
                  <ToggleGroup value={[field.value]} onValueChange={(v) => v[0] && field.onChange(v[0])} variant="outline" spacing={0} aria-label="페이지 매기기 유형">
                    <ToggleGroupItem value="all">모두</ToggleGroupItem>
                    <ToggleGroupItem value="pages">페이지 매기기</ToggleGroupItem>
                    <ToggleGroupItem value="more">더 보기</ToggleGroupItem>
                    <ToggleGroupItem value="auto_more">자동 더보기</ToggleGroupItem>
                  </ToggleGroup>
                )}
              />
              <FieldDescription>메인 저장소 목록과 폴더 목록에 함께 적용됩니다.</FieldDescription>
            </Field>
            <Field data-invalid={e.per_page ? true : undefined}>
              <FieldLabel htmlFor="per_page">페이지당 기본 항목 수</FieldLabel>
              <Input id="per_page" type="number" min={5} max={1000} className="w-32" aria-invalid={!!e.per_page} {...num("per_page")} />
              <FieldError errors={[e.per_page]} />
            </Field>
          </FieldGroup>
        </TabsContent>

        <TabsContent value="preview" className="pt-4">
          <FieldGroup className="max-w-xl">
            <SwitchField control={form.control} name="audio_autoplay" title="오디오 자동 재생" description="오디오 파일을 열면 바로 재생합니다." />
            <SwitchField control={form.control} name="video_autoplay" title="비디오 자동 재생" description="비디오 파일을 열면 바로 재생합니다." />
            <SwitchField
              control={form.control}
              name="archive_preview"
              title="압축 미리보기"
              description="ZIP, 7z, RAR, TAR 파일을 열면 내부 파일 목록을 보여줍니다. 끄면 다운로드 탭으로 엽니다."
            />
          </FieldGroup>
        </TabsContent>

        <TabsContent value="system" className="flex flex-col gap-6 pt-4">
          <FieldGroup className="max-w-xl">
            <Field>
              <FieldLabel>파일 전송 방식</FieldLabel>
              <Controller
                control={form.control}
                name="data_mode"
                render={({ field }) => (
                  <ToggleGroup value={[field.value]} onValueChange={(v) => v[0] && field.onChange(v[0])} variant="outline" spacing={0} aria-label="파일 전송 방식">
                    <ToggleGroupItem value="direct">직접 연결</ToggleGroupItem>
                    <ToggleGroupItem value="proxy">서버 경유</ToggleGroupItem>
                  </ToggleGroup>
                )}
              />
              <FieldDescription>
                직접 연결은 Dropbox 임시 링크로 받아 빠르고 서버 트래픽이 없지만 브라우저에 Dropbox 주소가 보입니다. 서버 경유는 모든 데이터가 이 서버를 거칩니다. 변환 재생과
                압축 다운로드는 항상 서버를 거칩니다.
              </FieldDescription>
            </Field>
            <NumberField id="cache_cleanup_sec" label="캐시 정리 주기 (초)" error={e.cache_cleanup_sec} reg={num("cache_cleanup_sec")} help="만료된 캐시 항목을 이 간격마다 지웁니다. 기본 120초." />
            <NumberField id="transcode_concurrency" label="동시 변환 수" error={e.transcode_concurrency} reg={num("transcode_concurrency")} help="ffmpeg 변환을 동시에 몇 개까지 돌릴지 정합니다. 0이면 변환 재생을 끕니다. Ampere A1 4코어에서는 2를 권장합니다." />
            <NumberField id="thumb_cache_mb" label="썸네일 캐시 용량 (MB)" error={e.thumb_cache_mb} reg={num("thumb_cache_mb")} help="넘으면 오래 안 본 썸네일부터 지웁니다." />
            <NumberField id="folder_size_concurrency" label="폴더 크기 동시 계산 수" error={e.folder_size_concurrency} reg={num("folder_size_concurrency")} help="우클릭 메뉴의 폴더 크기 계산을 서버 전체에서 동시에 몇 개까지 할지 정합니다." />
            <NumberField id="search_concurrency" label="검색 동시 요청 수" error={e.search_concurrency} reg={num("search_concurrency")} help="통합 검색 때 Dropbox 계정에 동시에 보내는 요청 수입니다." />
            <NumberField id="archive_max_mb" label="압축 파일 읽기 상한 (MB)" error={e.archive_max_mb} reg={num("archive_max_mb")} help="RAR, TAR.GZ처럼 처음부터 읽어야 하는 형식의 목록을 만들 때 읽을 최대 크기입니다." />
          </FieldGroup>
          <SystemStatus />
        </TabsContent>
      </Tabs>
    </form>
  )
}

function SwitchField({ control, name, title, description }: { control: Control<Settings>; name: "audio_autoplay" | "video_autoplay" | "archive_preview"; title: string; description: string }) {
  return (
    <Controller
      control={control}
      name={name}
      render={({ field }) => (
        <Field orientation="horizontal">
          <FieldContent>
            <FieldTitle>{title}</FieldTitle>
            <FieldDescription>{description}</FieldDescription>
          </FieldContent>
          <Switch checked={!!field.value} onCheckedChange={field.onChange} aria-label={title} />
        </Field>
      )}
    />
  )
}

function NumberField({ id, label, help, error, reg }: { id: string; label: string; help: string; error?: { message?: string }; reg: ReturnType<ReturnType<typeof useForm<Settings>>["register"]> }) {
  return (
    <Field data-invalid={error ? true : undefined}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <Input id={id} type="number" className="w-32" aria-invalid={!!error} {...reg} />
      <FieldDescription>{help}</FieldDescription>
      <FieldError errors={[error]} />
    </Field>
  )
}

function SystemStatus() {
  const sys = useQuery({ queryKey: ["system"], queryFn: api.system, refetchInterval: 5000 })
  const [busy, setBusy] = useState(false)
  const clear = async (thumbs: boolean) => {
    setBusy(true)
    try {
      await api.clearCache(thumbs)
      queryClient.invalidateQueries({ queryKey: ["system"] })
      queryClient.invalidateQueries({ queryKey: ["list"] })
      toast.add({ title: thumbs ? "캐시와 썸네일을 비웠습니다" : "캐시를 비웠습니다", type: "success", timeout: 2500 })
    } finally {
      setBusy(false)
    }
  }
  const s = sys.data
  const hitRate = s && s.cache.hits + s.cache.misses > 0 ? Math.round((s.cache.hits / (s.cache.hits + s.cache.misses)) * 100) : null
  return (
    <Card className="max-w-xl">
      <CardHeader>
        <CardTitle>캐시와 서버 상태</CardTitle>
        <CardDescription>5초마다 새로 고칩니다.</CardDescription>
      </CardHeader>
      <CardContent>
        {!s ? (
          <Spinner />
        ) : (
          <dl className="grid grid-cols-[10rem_1fr] gap-y-1.5 text-sm tabular-nums">
            <dt className="text-muted-foreground">메모리 캐시</dt>
            <dd>
              {s.cache.entries.toLocaleString()}개, 약 {formatBytes(s.cache.bytes)} / {formatBytes(s.cache.max_bytes)}
            </dd>
            <dt className="text-muted-foreground">적중률</dt>
            <dd>{hitRate === null ? "아직 없음" : `${hitRate}% (적중 ${s.cache.hits.toLocaleString()}, 실패 ${s.cache.misses.toLocaleString()})`}</dd>
            <dt className="text-muted-foreground">밀려나거나 만료됨</dt>
            <dd>
              {s.cache.evictions.toLocaleString()}개 / {s.cache.expirations.toLocaleString()}개
            </dd>
            <dt className="text-muted-foreground">썸네일 디스크 캐시</dt>
            <dd>
              {s.thumbs.files.toLocaleString()}개, {formatBytes(s.thumbs.bytes)} / {formatBytes(s.thumbs.max_bytes)}
            </dd>
            <dt className="text-muted-foreground">변환 재생</dt>
            <dd>{s.ffmpeg ? `${s.transcode.active}/${s.transcode.limit} 사용 중` : "ffmpeg 없음 (원본 재생만 가능)"}</dd>
            <dt className="text-muted-foreground">폴더 크기 계산</dt>
            <dd>
              {s.folder_size.active}/{s.folder_size.limit} 진행, {s.folder_size.waiting} 대기
            </dd>
            <dt className="text-muted-foreground">데이터 폴더</dt>
            <dd className="break-all">{s.data_dir}</dd>
          </dl>
        )}
      </CardContent>
      <CardFooter className="gap-2">
        <Button type="button" variant="outline" onClick={() => clear(false)} disabled={busy}>
          캐시 비우기
        </Button>
        <Button type="button" variant="outline" onClick={() => clear(true)} disabled={busy}>
          썸네일까지 비우기
        </Button>
      </CardFooter>
    </Card>
  )
}
