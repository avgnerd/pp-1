import { useQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { selectError, usePlayer } from "@videojs/react"
import { Hls, type HlsJsAdapter } from "@videojs/hlsjs-video"
import { Audio, AudioPlayer } from "@videojs/react/audio"
import { HlsAudio } from "@videojs/react/media/hls-audio"
import { HlsJsVideo } from "@videojs/react/media/hlsjs-video"
import { Video, VideoPlayer } from "@videojs/react/video"
import { GlassesIcon, GaugeIcon } from "lucide-react"
import { useCallback, useEffect, useMemo, useRef, useState } from "react"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Skeleton } from "@/components/ui/skeleton"
import { toast } from "@/components/ui/toast"
import { AudioSkin } from "@/components/videojs/audio/skin"
import { PlayerSlotsContext, type PlayerSlots } from "@/components/videojs/slots"
import { VideoSkin } from "@/components/videojs/video/skin"
import { api, urls, type Entry, type Probe } from "@/lib/api"
import { fileKind } from "@/lib/format"
import { toSplat } from "@/lib/paths"
import { useConfig } from "@/lib/queries"
import { cn } from "@/lib/utils"
import { usePrefs, type Quality } from "@/stores/ui"

import { convertHeight, decide, stutterRatio, type FrameSample, type Plan } from "./decide"
import { AutoNextButton, ChoiceSubmenu, NextButton, PotPlayerButton, PrevButton, WebFullscreenButton } from "./player-controls"
import { VRView, type Projection, type Stereo } from "./vr-view"

// Sessions are closed shortly after the player unmounts; a quick remount (React StrictMode,
// route re-render) reuses the same session instead.
const pendingClose = new Map<string, ReturnType<typeof setTimeout>>()
function scheduleClose(id: string) {
  pendingClose.set(id, setTimeout(() => {
    pendingClose.delete(id)
    api.hlsClose(id).catch(() => {})
  }, 2000))
}
function keepOpen(id: string) {
  const t = pendingClose.get(id)
  if (t) clearTimeout(t)
  pendingClose.delete(id)
}

type VRMode = "off" | `${Projection}-${Stereo}`

// hls.js gives up on a fragment after 10 s without a first byte by default. After a seek the
// server may need longer to start ffmpeg and encode the first segment (it waits up to 60 s).
// The server answers 410 for a gone session or a segment past the end. hls.js would retry those
// for about 30 s before giving up, so the player reacts to the first one (see onHlsMedia).
const loadPolicy = {
  default: {
    maxTimeToFirstByteMs: 45_000,
    maxLoadTimeMs: 120_000,
    timeoutRetry: { maxNumRetry: 2, retryDelayMs: 0, maxRetryDelayMs: 0 },
    errorRetry: { maxNumRetry: 6, retryDelayMs: 1000, maxRetryDelayMs: 8000 },
  },
}
const hlsEngine = { hlsJs: { fragLoadPolicy: loadPolicy } }

/** Reports fatal errors from the media's engine (hls.js), which never reach the element's onError. */
function EngineErrors({ onFatal }: { onFatal: (dismiss: () => void) => void | Promise<void> }) {
  const err = usePlayer(selectError)
  const error = err?.error
  const dismiss = err?.dismissError
  // Handle each error once, even if onFatal changes while it is still on screen.
  const handled = useRef<unknown>(null)
  useEffect(() => {
    if (!error || !dismiss || handled.current === error) return
    handled.current = error
    void onFatal(dismiss)
  }, [error, dismiss, onFatal])
  return null
}

function initialVR(p?: Probe): VRMode {
  if (!p?.vr?.projection) return "off"
  return `${p.vr.projection}-${p.vr.stereo || "mono"}` as VRMode
}

export default function MediaPlayer({ entry, siblings }: { entry: Entry; siblings: Entry[] }) {
  const cfg = useConfig()
  const navigate = useNavigate()
  const isAudio = fileKind(entry.name) === "audio"
  const { quality, setQuality, autoNext, setAutoNext, volume, muted, setVolume } = usePrefs()
  const transcode = !!cfg.data?.transcode

  const probe = useQuery({
    queryKey: ["probe", entry.path, entry.rev],
    queryFn: () => api.probe(entry.path),
    staleTime: Infinity,
    enabled: !!cfg.data?.probe,
  })
  const probeData = probe.data?.probe
  const waitingForProbe = !!cfg.data?.probe && probe.isPending

  const [fallback, setFallback] = useState<Plan | null>(null)
  const qNum = quality === "original" ? null : Number(quality)
  const plan: Plan = fallback ?? decide(entry.name, probeData, qNum, isAudio, transcode)
  // "convert" is a per-file choice in the quality menu; it is never saved as a preference.
  const converting = fallback?.kind === "hls" && fallback.mode === "transcode"

  const [vr, setVr] = useState<VRMode>("off")
  const vrTouched = useRef(false)
  useEffect(() => {
    if (!vrTouched.current && probeData) setVr(initialVR(probeData))
  }, [probeData])

  const hls = useQuery({
    queryKey: ["hls", entry.path, entry.rev, plan.kind === "hls" ? plan.mode : "", plan.kind === "hls" ? plan.height : 0],
    queryFn: () => api.hls(entry.path, (plan as Extract<Plan, { kind: "hls" }>).mode, (plan as Extract<Plan, { kind: "hls" }>).height || 1080),
    enabled: plan.kind === "hls" && !waitingForProbe,
    staleTime: Infinity,
    gcTime: 5_000,
    retry: false,
  })
  const hlsId = hls.data?.id
  const refetchHls = hls.refetch
  const videoRef = useRef<HTMLVideoElement>(null)
  const [video, setVideo] = useState<HTMLVideoElement | null>(null)
  const onVideo = useCallback((node: HTMLVideoElement | null) => {
    videoRef.current = node
    setVideo(node)
  }, [])
  const audioRef = useRef<HTMLAudioElement>(null)
  const resumeAt = useRef(0)
  // Media events keep playback intent current while asynchronous recovery waits.
  const resumePlay = useRef(false)
  const resumeRate = useRef(1)
  // Initial events from a replacement must not overwrite the position being restored.
  const restoringFrom = useRef<HTMLMediaElement | null>(null)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false }
  }, [])
  const [loadedOnce, setLoadedOnce] = useState(false)
  const keepPlaying = useCallback(() => {
    const el = videoRef.current ?? audioRef.current
    if (!el || (restoringFrom.current && restoringFrom.current !== el)) return
    resumeAt.current = el.currentTime
    resumePlay.current = !el.paused && !el.ended
    resumeRate.current = el.playbackRate
    restoringFrom.current = el
  }, [])
  useEffect(() => {
    if (!hlsId) return
    let cancelled = false
    keepOpen(hlsId)
    // Heartbeat: keeps the server session while this player is open, including while paused.
    // If the server already dropped it (restart, long sleep), open a new session; the player
    // resumes at the same position.
    const beat = setInterval(() => {
      api.hlsPing(hlsId).catch((e) => {
        if (cancelled || (e as { status?: number }).status !== 404) return
        keepPlaying()
        refetchHls()
      })
    }, 20_000)
    return () => {
      cancelled = true
      clearInterval(beat)
      scheduleClose(hlsId)
    }
  }, [hlsId, refetchHls, keepPlaying])

  // Previous / next among media files of the same folder.
  const media = useMemo(() => siblings.filter((e) => ["video", "audio"].includes(fileKind(e.name))), [siblings])
  const idx = media.findIndex((e) => e.path === entry.path)
  const prev = idx > 0 ? media[idx - 1] : undefined
  const next = idx >= 0 && idx < media.length - 1 ? media[idx + 1] : undefined
  const go = useCallback((e?: Entry) => e && navigate({ to: "/$", params: { _splat: toSplat(e.path) } }), [navigate])

  const [webFull, setWebFull] = useState(false)
  useEffect(() => {
    if (!webFull) return
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setWebFull(false)
    window.addEventListener("keydown", onKey)
    document.documentElement.style.overflow = "hidden"
    return () => {
      window.removeEventListener("keydown", onKey)
      document.documentElement.style.overflow = ""
    }
  }, [webFull])

  const openPotPlayer = async () => {
    try {
      const { url } = await api.link(entry.path)
      window.location.href = "potplayer://" + url
    } catch (e) {
      toast.add({ title: "팟플레이어 링크를 만들지 못했습니다", description: (e as Error).message, type: "error" })
    }
  }

  const vrOn = !isAudio && vr !== "off"
  const [projection, stereo] = (vrOn ? vr.split("-") : ["360", "mono"]) as [Projection, Stereo]
  // A fresh token asks the server for a new Dropbox link after the old one expired.
  const [fresh, setFresh] = useState("")
  const freshTries = useRef({ count: 0, at: 0 })
  const hlsRetries = useRef(0)
  const [failedSource, setFailedSource] = useState<string | null>(null)
  const dismissHlsError = useRef(() => {})
  const recoverySource = JSON.stringify([entry.path, entry.rev, plan.kind, plan.kind === "hls" ? plan.mode : "", plan.kind === "hls" ? plan.height : 0])
  useEffect(() => {
    hlsRetries.current = 0
  }, [recoverySource])
  // Bumped to reload the same HLS playlist after a fatal engine error.
  const [reload, setReload] = useState(0)
  const autoplay = isAudio ? !!cfg.data?.audio_autoplay : !!cfg.data?.video_autoplay
  // WebGL needs same-origin (or CORS) pixels, so VR always streams through the server.
  const src = plan.kind === "original" ? urls.raw(entry.path, { proxy: vrOn, fresh }) : hls.data?.playlist
  const playbackSource = JSON.stringify([recoverySource, src, reload])

  // Watch original playback for dropped frames and switch to server conversion once if the
  // browser cannot keep up with the stream. Skipped for VR (frames go to a canvas), hidden
  // tabs and paused/seeking playback, where browsers drop or skip frames on purpose.
  const autoSwitched = useRef(false)
  useEffect(() => {
    if (isAudio || vrOn || plan.kind !== "original" || !transcode || autoSwitched.current || !src) return
    let window: FrameSample[] = []
    const reset = () => (window = [])
    const v = videoRef.current
    v?.addEventListener("seeking", reset)
    v?.addEventListener("pause", reset)
    const timer = setInterval(() => {
      const el = videoRef.current
      if (!el || el.paused || el.seeking || el.readyState < 3 || document.visibilityState !== "visible" || !el.getVideoPlaybackQuality) {
        reset()
        return
      }
      const q = el.getVideoPlaybackQuality()
      window.push({ total: q.totalVideoFrames, dropped: q.droppedVideoFrames })
      if (window.length > 8) window.shift()
      const ratio = stutterRatio(window)
      if (ratio === null) return
      autoSwitched.current = true
      clearInterval(timer)
      const pct = Math.round(ratio * 100)
      keepPlaying()
      setFallback({ kind: "hls", mode: "transcode", height: convertHeight(probeData), reason: `원본 재생 중 프레임이 ${pct}% 끊겨 다시 인코딩` })
      toast.add({
        title: `프레임이 ${pct}% 끊겨 변환 재생으로 바꿉니다`,
        description: "보던 위치에서 이어서 재생합니다. 설정 메뉴의 화질에서 원본으로 되돌릴 수 있습니다.",
        type: "info",
        timeout: 5000,
      })
    }, 1000)
    return () => {
      clearInterval(timer)
      v?.removeEventListener("seeking", reset)
      v?.removeEventListener("pause", reset)
    }
  }, [isAudio, vrOn, plan.kind, transcode, src, probeData, keepPlaying])

  const mediaEvents = {
    onPlay: () => { resumePlay.current = true },
    onPause: (e: React.SyntheticEvent<HTMLMediaElement>) => {
      const el = e.currentTarget
      if (el.isConnected && el.readyState > 0 && !el.error) resumePlay.current = false
    },
    onSeeking: (e: React.SyntheticEvent<HTMLMediaElement>) => {
      if (!restoringFrom.current || restoringFrom.current === e.currentTarget) resumeAt.current = e.currentTarget.currentTime
    },
    onTimeUpdate: (e: React.SyntheticEvent<HTMLMediaElement>) => {
      if (!restoringFrom.current || restoringFrom.current === e.currentTarget) resumeAt.current = e.currentTarget.currentTime
    },
    onRateChange: (e: React.SyntheticEvent<HTMLMediaElement>) => {
      const el = e.currentTarget
      if (el === (videoRef.current ?? audioRef.current) && el.readyState > 0 && (!restoringFrom.current || restoringFrom.current === el)) {
        resumeRate.current = el.playbackRate
      }
    },
    onLoadedMetadata: (e: React.SyntheticEvent<HTMLMediaElement>) => {
      const el = e.currentTarget
      if (el === (videoRef.current ?? audioRef.current)) setFailedSource(null)
      el.volume = volume
      el.muted = muted
      el.playbackRate = resumeRate.current
      if (resumeAt.current > 0) el.currentTime = resumeAt.current
      restoringFrom.current = null
      if (resumePlay.current) {
        el.play().catch(() => {})
      }
      setLoadedOnce(true)
    },
    onVolumeChange: (e: React.SyntheticEvent<HTMLMediaElement>) => setVolume(e.currentTarget.volume, e.currentTarget.muted),
    onEnded: () => {
      if (autoNext && next) go(next)
    },
    onError: (e: React.SyntheticEvent<HTMLMediaElement>) => {
      // Converted playback errors arrive through EngineErrors.
      if (plan.kind !== "original") return
      const el = e.currentTarget
      const code = el.error?.code
      // Network error during original playback: usually the Dropbox temporary link expired
      // (valid about four hours). Reload from the same position with a new link. Allowed
      // twice in a row; the count resets after ten minutes of playback.
      const ft = freshTries.current
      if (Date.now() - ft.at > 10 * 60_000) ft.count = 0
      if (code === MediaError.MEDIA_ERR_NETWORK && ft.count < 2) {
        ft.count++
        ft.at = Date.now()
        resumeAt.current = el.currentTime
        restoringFrom.current = el
        setFresh(Date.now().toString(36))
        return
      }
      // Original playback failed (codec/container): switch to server transcoding once.
      if (transcode && !fallback) {
        resumeAt.current = el.currentTime
        restoringFrom.current = el
        setFallback(isAudio ? { kind: "hls", mode: "audio", height: 0, reason: "원본 재생 실패" } : { kind: "hls", mode: "transcode", height: convertHeight(probeData), reason: "원본 재생 실패" })
        toast.add({ title: "원본을 재생할 수 없어 변환 재생으로 바꿉니다", type: "info", timeout: 3500 })
      }
    },
  }

  const recoverHls = useCallback(
    async (dismiss: () => void, reportTerminal?: () => void, mode: "automatic" | "manual" = "automatic") => {
      keepPlaying()
      const previous = videoRef.current ?? audioRef.current
      if (hlsRetries.current >= 2) {
        setFailedSource(playbackSource)
        reportTerminal?.()
        return
      }
      if (hlsId) {
        await api.hlsPing(hlsId).catch(() => {})
      }
      if (!mounted.current || previous !== (videoRef.current ?? audioRef.current)) return
      if (mode === "automatic") hlsRetries.current++
      dismiss()
      const result = await refetchHls()
      if (mounted.current && previous === (videoRef.current ?? audioRef.current) && result.data?.playlist === src) {
        setReload((r) => r + 1)
      }
    },
    [hlsId, refetchHls, keepPlaying, src, playbackSource],
  )
  // Several requests can fail at once (init and a segment); recover once.
  const recovering = useRef(false)
  const onEngineFatal = useCallback(
    async (dismiss: () => void, reportTerminal?: () => void, mode: "automatic" | "manual" = "automatic") => {
      dismissHlsError.current = dismiss
      if (plan.kind !== "hls" || recovering.current) return
      recovering.current = true
      try {
        await recoverHls(dismiss, reportTerminal, mode)
      } finally {
        recovering.current = false
      }
    },
    [plan.kind, recoverHls],
  )
  const retryHls = () => {
    hlsRetries.current = 0
    setFailedSource(null)
    void onEngineFatal(dismissHlsError.current, undefined, "manual")
  }
  const onGone = useRef<(reportTerminal: () => void) => void>(() => {})
  useEffect(() => {
    onGone.current = (reportTerminal) => void onEngineFatal(() => {}, reportTerminal)
  }, [onEngineFatal])
  // A 410 cannot get better by retrying: stop hls.js and recover right away. The engine exists
  // only once the adapter starts loading, shortly after it is attached.
  const hlsMedia = useRef<HlsJsAdapter | null>(null)
  const onHlsMedia = useCallback((m: HlsJsAdapter | null) => {
    hlsMedia.current = m
    if (!m) return
    let off = () => {}
    let timer: ReturnType<typeof setTimeout> | undefined
    const attach = (tries: number) => {
      const engine = m.engine
      if (!engine) {
        if (tries < 100) timer = setTimeout(() => attach(tries + 1), 50)
        return
      }
      const onErr: Parameters<typeof engine.on<typeof Hls.Events.ERROR>>[1] = (_e, data) => {
        if (hlsMedia.current !== m || data.fatal || data.response?.code !== 410) return
        engine.stopLoad()
        onGone.current(() => engine.trigger(Hls.Events.ERROR, {
          ...data,
          fatal: true,
          type: Hls.ErrorTypes.OTHER_ERROR,
          error: new Error("재생할 내용을 불러오지 못했습니다. '다시 재생'을 눌러 보던 위치에서 다시 시도하세요."),
        }))
      }
      engine.on(Hls.Events.ERROR, onErr)
      off = () => engine.off(Hls.Events.ERROR, onErr)
    }
    attach(0)
    return () => {
      clearTimeout(timer)
      off()
    }
  }, [])

  const srcH = probeData?.video?.height ?? 0
  const qualityChoices = [
    { value: "original", label: "원본", hint: srcH ? `${srcH}p` : undefined },
    ...(transcode
      ? [
          { value: "convert", label: "변환 재생", hint: `${convertHeight(probeData)}p, 끊길 때` },
          ...(srcH === 0 || srcH > 1080 ? [{ value: "1080", label: "1080p" }] : []),
          ...(srcH === 0 || srcH > 720 ? [{ value: "720", label: "720p" }] : []),
        ]
      : []),
  ]

  const slots: PlayerSlots = {
    retryPlayback: plan.kind === "hls" && failedSource === playbackSource ? retryHls : undefined,
    beforePlay: <PrevButton onClick={() => go(prev)} disabled={!prev} />,
    afterPlay: <NextButton onClick={() => go(next)} disabled={!next} />,
    end: (
      <>
        <AutoNextButton on={autoNext} onToggle={() => setAutoNext(!autoNext)} />
        <PotPlayerButton onClick={openPotPlayer} />
        {!isAudio ? <WebFullscreenButton on={webFull} onToggle={() => setWebFull(!webFull)} /> : null}
      </>
    ),
    settings: isAudio ? null : (
      <>
        <ChoiceSubmenu
          icon={<GaugeIcon />}
          title="화질"
          value={converting ? "convert" : quality}
          choices={qualityChoices}
          onChange={(v) => {
            keepPlaying()
            if (v === "convert") {
              autoSwitched.current = true
              setFallback({ kind: "hls", mode: "transcode", height: convertHeight(probeData), reason: "변환 재생을 직접 선택" })
              return
            }
            // Choosing a quality again also cancels an automatic switch for this file.
            autoSwitched.current = true
            setFallback(null)
            setQuality(v as Quality)
          }}
        />
        <ChoiceSubmenu
          icon={<GlassesIcon />}
          title="VR"
          value={vr}
          choices={[
            { value: "off", label: "일반 영상" },
            { value: "360-mono", label: "360°", hint: "단일" },
            { value: "360-sbs", label: "360°", hint: "좌우" },
            { value: "360-tb", label: "360°", hint: "상하" },
            { value: "180-sbs", label: "180°", hint: "좌우" },
            { value: "180-mono", label: "180°", hint: "단일" },
            { value: "180-tb", label: "180°", hint: "상하" },
          ]}
          onChange={(v) => {
            keepPlaying()
            vrTouched.current = true
            setVr(v as VRMode)
          }}
        />
      </>
    ),
    layers: vrOn ? <VRView video={video} projection={projection} stereo={stereo} /> : null,
  }

  const notice =
    plan.kind === "hls" ? (
      <p className="text-sm text-muted-foreground">
        서버에서 변환해 재생합니다: {plan.reason}. 처음 재생과 구간 이동에 몇 초 걸릴 수 있습니다.
      </p>
    ) : vrOn && probeData?.vr?.source ? (
      <p className="text-sm text-muted-foreground">
        {probeData.vr.source === "metadata" ? "영상 메타데이터" : "파일 이름"}로 VR 영상을 감지했습니다. 설정 메뉴의 VR에서 바꿀 수 있습니다.
      </p>
    ) : null

  if (waitingForProbe) return <Skeleton className={isAudio ? "h-24 w-full" : "aspect-video w-full"} />

  if (plan.kind === "hls" && hls.isError) {
    return (
      <Alert variant="destructive">
        <AlertTitle>변환 재생을 시작하지 못했습니다</AlertTitle>
        <AlertDescription>{(hls.error as Error).message}</AlertDescription>
      </Alert>
    )
  }

  // Autoplay applies to the first load only; later source swaps resume via resumePlay.
  const autoPlay = autoplay && !loadedOnce
  const mediaKey = `${src}#${reload}`
  const engineErrors = plan.kind === "hls" ? <EngineErrors onFatal={onEngineFatal} /> : null

  if (isAudio) {
    return (
      <div className="flex flex-col gap-2">
        <PlayerSlotsContext.Provider value={slots}>
          <AudioPlayer title={entry.name}>
            <AudioSkin className="w-full" style={{ "--media-accent-color": "var(--primary)" } as React.CSSProperties}>
              {!src ? null : plan.kind === "hls" ? (
                <HlsAudio key={mediaKey} ref={audioRef} src={src} autoPlay={autoPlay} {...mediaEvents} />
              ) : (
                <Audio key={mediaKey} ref={audioRef} src={src} autoPlay={autoPlay} preload="metadata" {...mediaEvents} />
              )}
            </AudioSkin>
            {engineErrors}
          </AudioPlayer>
        </PlayerSlotsContext.Provider>
        {notice}
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-2">
      <div className={cn(webFull && "fixed inset-0 z-50 grid place-items-center bg-black")}>
        <PlayerSlotsContext.Provider value={slots}>
          <VideoPlayer title={entry.name}>
            <VideoSkin
              className={cn("w-full overflow-hidden", webFull ? "h-svh" : "aspect-video max-h-[80svh] rounded-lg")}
              style={{ "--media-accent-color": "var(--primary)", "--media-accent-text-color": "var(--primary-foreground)" } as React.CSSProperties}
            >
              {!src ? null : plan.kind === "hls" ? (
                <HlsJsVideo key={mediaKey} ref={onVideo} mediaRef={onHlsMedia} source={{ src, engine: hlsEngine }} playsInline autoPlay={autoPlay} className={cn(vrOn && "opacity-0")} {...mediaEvents} />
              ) : (
                <Video
                  key={mediaKey}
                  ref={onVideo}
                  src={src}
                  playsInline
                  autoPlay={autoPlay}
                  preload="metadata"
                  crossOrigin={vrOn ? "anonymous" : undefined}
                  className={cn(vrOn && "opacity-0")}
                  {...mediaEvents}
                />
              )}
            </VideoSkin>
            {engineErrors}
          </VideoPlayer>
        </PlayerSlotsContext.Provider>
      </div>
      {notice}
    </div>
  )
}
