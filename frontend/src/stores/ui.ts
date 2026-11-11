import { create } from "zustand"
import { persist } from "zustand/middleware"

export type ViewMode = "list" | "grid"
export type SortKey = "name" | "size" | "modified"
export type SortOrder = "asc" | "desc"

export type Quality = "original" | "1080" | "720"

interface PrefsState {
  view: ViewMode
  sort: SortKey
  order: SortOrder
  autoNext: boolean
  quality: Quality
  volume: number
  muted: boolean
  setView: (v: ViewMode) => void
  toggleSort: (k: SortKey) => void
  setAutoNext: (v: boolean) => void
  setQuality: (q: Quality) => void
  setVolume: (volume: number, muted: boolean) => void
}

/** Display preferences, remembered in localStorage. */
export const usePrefs = create<PrefsState>()(
  persist(
    (set) => ({
      view: "list",
      sort: "name",
      order: "asc",
      autoNext: true,
      quality: "original",
      volume: 1,
      muted: false,
      setView: (view) => set({ view }),
      // Clicking the active column flips the order; another column starts ascending.
      toggleSort: (k) => set((s) => (s.sort === k ? { order: s.order === "asc" ? "desc" : "asc" } : { sort: k, order: "asc" })),
      setAutoNext: (autoNext) => set({ autoNext }),
      setQuality: (quality) => set({ quality }),
      setVolume: (volume, muted) => set({ volume, muted }),
    }),
    { name: "dvfs-prefs" },
  ),
)

interface SelectionState {
  dir: string
  selected: Set<string>
  toggle: (path: string) => void
  setMany: (paths: string[], on: boolean) => void
  clear: () => void
  resetFor: (dir: string) => void
}

/** Checkbox selection, scoped to one folder at a time. */
export const useSelection = create<SelectionState>()((set) => ({
  dir: "",
  selected: new Set(),
  toggle: (p) =>
    set((s) => {
      const n = new Set(s.selected)
      if (n.has(p)) n.delete(p)
      else n.add(p)
      return { selected: n }
    }),
  setMany: (paths, on) =>
    set((s) => {
      const n = new Set(s.selected)
      for (const p of paths) {
        if (on) n.add(p)
        else n.delete(p)
      }
      return { selected: n }
    }),
  clear: () => set({ selected: new Set() }),
  resetFor: (dir) => set((s) => (s.dir === dir ? s : { dir, selected: new Set() })),
}))
