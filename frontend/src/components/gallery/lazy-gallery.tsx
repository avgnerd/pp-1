import { lazy, Suspense, useState } from "react"

import type { Entry } from "@/lib/api"

const ImageGallery = lazy(() => import("./image-gallery"))

interface Props {
  images: Entry[]
  index: number | null
  onIndexChange: (i: number) => void
  onClose: () => void
}

/** Loads lightGallery (and its CSS) the first time an image is opened, then keeps it mounted. */
export function LazyGallery(props: Props) {
  const [wanted, setWanted] = useState(false)
  if (props.index !== null && !wanted) setWanted(true)
  if (!wanted) return null
  return (
    <Suspense fallback={null}>
      <ImageGallery {...props} />
    </Suspense>
  )
}

export { rememberRatio } from "./ratios"
