import { useEffect, useRef, useState } from "react"

export type Projection = "180" | "360"
export type Stereo = "mono" | "sbs" | "tb"

interface Props {
  video: HTMLVideoElement | null
  projection: Projection
  stereo: Stereo
}

/**
 * Renders the playing <video> onto the inside of a sphere (360°) or hemisphere (180°) with
 * three.js. Drag or arrow keys look around; the mouse wheel zooms by changing the field of view.
 * Stereo files show the left eye (left half for side-by-side, top half for top-bottom).
 */
export function VRView({ video, projection, stereo }: Props) {
  const hostRef = useRef<HTMLDivElement>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const host = hostRef.current
    if (!host || !video) return
    let disposed = false
    let cleanup = () => {}

    import("three").then((THREE) => {
      if (disposed) return
      setError(null)
      const renderer = new THREE.WebGLRenderer({ antialias: true, alpha: false, powerPreference: "high-performance" })
      renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2))
      renderer.outputColorSpace = THREE.SRGBColorSpace
      host.appendChild(renderer.domElement)
      renderer.domElement.style.display = "block"
      renderer.domElement.style.width = "100%"
      renderer.domElement.style.height = "100%"
      renderer.domElement.tabIndex = 0
      renderer.domElement.setAttribute("aria-label", "VR 영상: 드래그하거나 방향키로 둘러보고, 휠로 확대·축소")

      const scene = new THREE.Scene()
      const camera = new THREE.PerspectiveCamera(75, 16 / 9, 0.1, 1100)
      const texture = new THREE.VideoTexture(video)
      texture.colorSpace = THREE.SRGBColorSpace
      texture.minFilter = THREE.LinearFilter
      texture.magFilter = THREE.LinearFilter
      if (stereo === "sbs") {
        texture.repeat.set(0.5, 1)
        texture.offset.set(0, 0)
      } else if (stereo === "tb") {
        texture.repeat.set(1, 0.5)
        texture.offset.set(0, 0.5)
      }
      const geometry =
        projection === "180" ? new THREE.SphereGeometry(500, 64, 48, Math.PI, Math.PI) : new THREE.SphereGeometry(500, 64, 48)
      geometry.scale(-1, 1, 1)
      const mesh = new THREE.Mesh(geometry, new THREE.MeshBasicMaterial({ map: texture }))
      scene.add(mesh)

      let lon = 0
      let lat = 0
      // Skip repainting a paused, untouched video; on software-rendered machines that work starves
      // the page. Playback, camera input, layout changes and decoded frames mark the scene dirty.
      const frameDriven = "requestVideoFrameCallback" in video
      let dirty = true
      let frameRequest: number | undefined
      const watchFrames = () => {
        if (!frameDriven) return
        frameRequest = video.requestVideoFrameCallback(() => {
          dirty = true
          watchFrames()
        })
      }
      watchFrames()
      const maxLon = projection === "180" ? 85 : Infinity
      const clamp = () => {
        lat = Math.max(-85, Math.min(85, lat))
        if (Number.isFinite(maxLon)) lon = Math.max(-maxLon, Math.min(maxLon, lon))
      }
      const resize = () => {
        const w = host.clientWidth || 1
        const h = host.clientHeight || 1
        renderer.setSize(w, h, false)
        camera.aspect = w / h
        camera.updateProjectionMatrix()
        dirty = true
      }
      const ro = new ResizeObserver(resize)
      ro.observe(host)
      resize()

      let drag: { x: number; y: number; lon: number; lat: number; id: number } | null = null
      const el = renderer.domElement
      const onDown = (e: PointerEvent) => {
        drag = { x: e.clientX, y: e.clientY, lon, lat, id: e.pointerId }
        el.setPointerCapture(e.pointerId)
      }
      const onMove = (e: PointerEvent) => {
        if (!drag || drag.id !== e.pointerId) return
        const k = camera.fov / host.clientHeight
        lon = drag.lon - (e.clientX - drag.x) * k
        lat = drag.lat + (e.clientY - drag.y) * k
        clamp()
        dirty = true
      }
      const onUp = (e: PointerEvent) => {
        if (drag?.id === e.pointerId) drag = null
      }
      const onWheel = (e: WheelEvent) => {
        e.preventDefault()
        e.stopPropagation()
        camera.fov = Math.max(25, Math.min(110, camera.fov + Math.sign(e.deltaY) * 4))
        camera.updateProjectionMatrix()
        dirty = true
      }
      const onKey = (e: KeyboardEvent) => {
        const step = 5
        if (e.key === "ArrowLeft") lon -= step
        else if (e.key === "ArrowRight") lon += step
        else if (e.key === "ArrowUp") lat += step
        else if (e.key === "ArrowDown") lat -= step
        else if (e.key === "+" || e.key === "=") camera.fov = Math.max(25, camera.fov - 5)
        else if (e.key === "-") camera.fov = Math.min(110, camera.fov + 5)
        else return
        e.preventDefault()
        e.stopPropagation()
        clamp()
        camera.updateProjectionMatrix()
        dirty = true
      }
      el.addEventListener("pointerdown", onDown)
      el.addEventListener("pointermove", onMove)
      el.addEventListener("pointerup", onUp)
      el.addEventListener("pointercancel", onUp)
      el.addEventListener("wheel", onWheel, { passive: false })
      el.addEventListener("keydown", onKey)

      const target = new THREE.Vector3()
      renderer.setAnimationLoop(() => {
        // Without frame callbacks a paused seek has no invalidation signal, so keep repainting.
        if (!dirty && video.paused && frameDriven) return
        dirty = false
        const phi = THREE.MathUtils.degToRad(90 - lat)
        const theta = THREE.MathUtils.degToRad(lon - 90)
        target.set(Math.sin(phi) * Math.cos(theta), Math.cos(phi), Math.sin(phi) * Math.sin(theta))
        camera.lookAt(target)
        try {
          renderer.render(scene, camera)
        } catch {
          setError("이 영상은 브라우저 보안 정책(CORS) 때문에 VR로 그릴 수 없습니다.")
          renderer.setAnimationLoop(null)
        }
      })

      cleanup = () => {
        renderer.setAnimationLoop(null)
        if (frameRequest !== undefined) video.cancelVideoFrameCallback(frameRequest)
        ro.disconnect()
        el.removeEventListener("pointerdown", onDown)
        el.removeEventListener("pointermove", onMove)
        el.removeEventListener("pointerup", onUp)
        el.removeEventListener("pointercancel", onUp)
        el.removeEventListener("wheel", onWheel)
        el.removeEventListener("keydown", onKey)
        texture.dispose()
        geometry.dispose()
        ;(mesh.material as { dispose(): void }).dispose()
        renderer.dispose()
        el.remove()
      }
    })
    return () => {
      disposed = true
      cleanup()
    }
  }, [video, projection, stereo])

  return (
    <div ref={hostRef} className="absolute inset-0 z-[1] bg-black" data-vr={projection}>
      {error ? <p className="absolute inset-x-4 top-4 rounded-md bg-black/70 p-3 text-sm text-white">{error}</p> : null}
    </div>
  )
}
