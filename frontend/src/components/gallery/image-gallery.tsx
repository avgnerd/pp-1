import { LightGallery } from "@lightgallery/react"
import Thumbnail from "@lightgallery/react/plugins/thumbnail"
import Zoom from "@lightgallery/react/plugins/zoom"
import "lightgallery/css/lightgallery.css"
import "lightgallery/css/lg-thumbnail.css"
import "lightgallery/css/lg-zoom.css"
import {
  ChevronLeft,
  ChevronRight,
  Ellipsis,
  LayoutGrid,
  Maximize2,
  Minimize2,
  X,
  ZoomIn,
  ZoomOut,
} from "lucide-react"
import { useEffect, useMemo } from "react"

import { urls, type Entry } from "@/lib/api"

import { lgSizeFor } from "./ratios"

/**
 * The 3.x zoom plugin zooms with buttons, double-click and pinch but not the mouse wheel.
 * While the gallery is open we translate wheel notches into its zoom-in / zoom-out buttons.
 */
function useWheelZoom(active: boolean) {
  useEffect(() => {
    if (!active) return
    let last = 0
    const onWheel = (e: WheelEvent) => {
      const target = e.target as Element | null
      const container = target?.closest?.(".lg-container")
      if (!container || target?.closest(".lg-thumb-outer")) return
      e.preventDefault()
      const now = performance.now()
      if (now - last < 90) return
      last = now
      const btn = container.querySelector<HTMLButtonElement>(e.deltaY < 0 ? ".lg-zoom-in" : ".lg-zoom-out")
      btn?.click()
    }
    document.addEventListener("wheel", onWheel, { passive: false, capture: true })
    return () => document.removeEventListener("wheel", onWheel, { capture: true })
  }, [active])
}

interface Props {
  images: Entry[]
  index: number | null
  onIndexChange: (i: number) => void
  onClose: () => void
}

/** Controlled lightGallery over the images of the current folder (always controlled). */
export default function ImageGallery({ images, index, onIndexChange, onClose }: Props) {
  const open = index !== null
  useWheelZoom(open)
  const slides = useMemo(
    () =>
      images.map((e) => ({
        src: urls.raw(e.path),
        thumb: urls.thumb(e.path, e.rev, "w128h128"),
        lgSize: lgSizeFor(e.path),
        alt: e.name,
        subHtml: e.name,
      })),
    // Ratios are read when the slide list is built, i.e. when the gallery opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [images, open],
  )
  return (
    <LightGallery
      plugins={[Zoom, Thumbnail]}
      zoom={{
        scale: 0.5,
        showZoomInOutIcons: true,
        actualSize: true,
        actualSizeIcons: {
          zoomIn: "lg-actual-size",
          zoomOut: "lg-actual-size",
        },
        infiniteZoom: true,
      }}
      thumbnail={{ animateThumb: true }}
      render={{
        icon: (name) => {
          switch (name) {
            case "zoomIn":
              return (
                <>
                  <ZoomIn className="lg-zoom-btn-icon" />
                  <Maximize2 className="lg-actual-btn-icon" />
                </>
              )
            case "zoomOut":
              return (
                <>
                  <ZoomOut className="lg-zoom-btn-icon" />
                  <Minimize2 className="lg-actual-btn-icon" />
                </>
              )
            case "close":
              return <X />
            case "prev":
              return <ChevronLeft />
            case "next":
              return <ChevronRight />
            case "toggleThumbnails":
              return <LayoutGrid />
            case "more":
              return <Ellipsis />
            default:
              return undefined
          }
        },
      }}
      slides={slides}
      open={open}
      index={index ?? 0}
      onIndexChange={onIndexChange}
      onClose={onClose}
      download={false}
      counter
      strings={{
        closeGallery: "닫기",
        previousSlide: "이전 사진",
        nextSlide: "다음 사진",
        zoomIn: "확대",
        zoomOut: "축소",
        viewActualSize: "원본 크기",
        toggleThumbnails: "썸네일 보기",
        galleryLabel: "사진 보기",
        slideAnnouncement: "{total}장 중 {index}번째",
        mediaLoadingFailed: "사진을 불러오지 못했습니다",
      }}
    />
  )
}
