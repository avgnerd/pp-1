/**
 * Natural aspect ratios learned from loaded thumbnails, keyed by path. lightGallery's
 * zoom-from-origin animation needs lgSize; we scale the thumbnail ratio up to a 2048px edge.
 */
const ratios = new Map<string, { w: number; h: number }>()

export function rememberRatio(path: string, img: HTMLImageElement) {
  if (img.naturalWidth > 1 && img.naturalHeight > 1) ratios.set(path, { w: img.naturalWidth, h: img.naturalHeight })
}

export function lgSizeFor(path: string): string | undefined {
  const r = ratios.get(path)
  if (!r) return undefined
  const scale = 2048 / Math.max(r.w, r.h)
  return `${Math.round(r.w * scale)}-${Math.round(r.h * scale)}`
}
