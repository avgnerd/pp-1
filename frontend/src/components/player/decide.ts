import type { Probe } from "@/lib/api"
import { extOf } from "@/lib/format"

export type Plan =
  | { kind: "original" }
  | { kind: "hls"; mode: "remux" | "transcode" | "audio"; height: number; reason: string }

// Containers browsers play natively through a plain <video>/<audio> src.
const nativeVideo = new Set(["mp4", "m4v", "webm", "mov", "ogv"])
const nativeAudio = new Set(["mp3", "m4a", "aac", "wav", "ogg", "oga", "opus", "flac", "webm"])

const videoCodec: Record<string, string> = {
  h264: 'avc1.640028',
  hevc: 'hvc1.1.6.L120.90',
  vp9: 'vp09.00.40.08',
  vp8: 'vp8',
  av1: 'av01.0.08M.08',
}
const audioCodec: Record<string, string> = {
  aac: "mp4a.40.2",
  mp3: "mp4a.6B",
  opus: "opus",
  flac: "flac",
  vorbis: "vorbis",
  ac3: "ac-3",
  eac3: "ec-3",
  alac: "alac",
}

function mse(type: string): boolean {
  try {
    const MS = (window as unknown as { ManagedMediaSource?: typeof MediaSource }).ManagedMediaSource ?? window.MediaSource
    return !!MS?.isTypeSupported?.(type)
  } catch {
    return false
  }
}

function canVideo(codec?: string): boolean {
  if (!codec) return true
  const c = videoCodec[codec]
  if (!c) return false
  return mse(`video/mp4; codecs="${c}"`) || document.createElement("video").canPlayType(`video/mp4; codecs="${c}"`) !== ""
}

function canAudio(codec?: string): boolean {
  if (!codec) return true
  const c = audioCodec[codec]
  if (!c) return false
  const a = document.createElement("audio")
  return mse(`audio/mp4; codecs="${c}"`) || a.canPlayType(`audio/mp4; codecs="${c}"`) !== "" || (codec === "mp3" && a.canPlayType("audio/mpeg") !== "")
}

/**
 * Original playback first. If only the container is unsupported (e.g. H.264 in MKV), remux to
 * HLS. If the codec is unsupported (WMV/VC-1, MPEG-4 Part 2...) or a lower quality was chosen,
 * transcode. Without probe data, try the original and let the error handler fall back.
 */
export function decide(name: string, probe: Probe | undefined, quality: number | null, isAudio: boolean, transcode: boolean): Plan {
  const ext = extOf(name)
  if (isAudio || (probe && !probe.video)) {
    if (!probe?.audio) return { kind: "original" }
    const native = nativeAudio.has(ext) || nativeVideo.has(ext)
    if (native && canAudio(probe.audio.codec_name)) return { kind: "original" }
    if (!transcode) return { kind: "original" }
    return { kind: "hls", mode: "audio", height: 0, reason: `${probe.audio.codec_name} 오디오를 AAC로 변환` }
  }
  const v = probe?.video
  if (quality && v?.height && v.height > quality && transcode) {
    return { kind: "hls", mode: "transcode", height: quality, reason: `${v.height}p → ${quality}p 변환` }
  }
  if (!probe || !v) return { kind: "original" }
  const vOK = canVideo(v.codec_name) && !(v.pix_fmt?.includes("422") || v.pix_fmt?.includes("444"))
  const aOK = canAudio(probe.audio?.codec_name)
  const containerOK = nativeVideo.has(ext) && (ext !== "mov" || v.codec_name === "h264" || v.codec_name === "hevc")
  if (containerOK && vOK && aOK) return { kind: "original" }
  if (!transcode) return { kind: "original" }
  if (vOK) return { kind: "hls", mode: "remux", height: 0, reason: `${ext.toUpperCase()} 컨테이너를 HLS로 다시 담음` }
  return { kind: "hls", mode: "transcode", height: Math.min(1080, v.height || 1080), reason: `${v.codec_name} 코덱을 H.264로 변환` }
}

/** Height used when the user forces server-side conversion: the source height, capped at 1080p. */
export function convertHeight(probe: Probe | undefined): number {
  const h = probe?.video?.height ?? 0
  return h > 0 ? Math.min(1080, h) : 1080
}

export interface FrameSample {
  total: number
  dropped: number
}

/** Minimum frames in the window before judging (about 4 s at 30 fps). */
export const DROP_MIN_FRAMES = 120
/** Share of dropped frames that counts as stuttering. */
export const DROP_RATIO = 0.1

/**
 * Decides from cumulative VideoPlaybackQuality counters (oldest first) whether original
 * playback is stuttering. Some files decode in a browser but lose frames, e.g. an H.264 SPS
 * that declares fewer reorder frames than the stream uses. Returns the drop ratio when it
 * exceeds DROP_RATIO over at least DROP_MIN_FRAMES, otherwise null.
 */
export function stutterRatio(window: FrameSample[]): number | null {
  if (window.length < 2) return null
  const a = window[0]
  const b = window[window.length - 1]
  const total = b.total - a.total
  const dropped = b.dropped - a.dropped
  if (total < DROP_MIN_FRAMES || dropped < 0) return null
  const ratio = dropped / total
  return ratio > DROP_RATIO ? ratio : null
}
