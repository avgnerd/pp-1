import { Menu, Text as TextPrimitive } from "@videojs/react"
import { Maximize2Icon, Minimize2Icon, MonitorPlayIcon, RepeatIcon, Repeat1Icon, SkipBackIcon, SkipForwardIcon } from "lucide-react"
import type { ReactNode } from "react"

import { Button } from "@/components/videojs/ui/button"
import { ButtonTooltip } from "@/components/videojs/ui/button-tooltip"
import { MenuChevron } from "@/components/videojs/ui/menu-chevron"
import { RadioItem } from "@/components/videojs/ui/radio-item"
import { cn } from "@/lib/utils"

const iconCls = "size-media-icon drop-shadow-media-icon [text-shadow:inherit]"

export function IconButton({ label, onClick, disabled, pressed, children, className }: { label: string; onClick: () => void; disabled?: boolean; pressed?: boolean; children: ReactNode; className?: string }) {
  return (
    <ButtonTooltip side="top" label={label}>
      <Button
        type="button"
        aria-label={label}
        aria-pressed={pressed}
        aria-disabled={disabled || undefined}
        onClick={() => !disabled && onClick()}
        className={className}
      >
        {children}
      </Button>
    </ButtonTooltip>
  )
}

export function PrevButton({ onClick, disabled }: { onClick: () => void; disabled: boolean }) {
  return (
    <IconButton label="이전 파일" onClick={onClick} disabled={disabled}>
      <SkipBackIcon className={iconCls} fill="currentColor" />
    </IconButton>
  )
}

export function NextButton({ onClick, disabled }: { onClick: () => void; disabled: boolean }) {
  return (
    <IconButton label="다음 파일" onClick={onClick} disabled={disabled}>
      <SkipForwardIcon className={iconCls} fill="currentColor" />
    </IconButton>
  )
}

export function AutoNextButton({ on, onToggle }: { on: boolean; onToggle: () => void }) {
  return (
    <IconButton label={on ? "다음 파일 자동 재생: 켜짐" : "다음 파일 자동 재생: 꺼짐"} onClick={onToggle} pressed={on} className={cn(!on && "opacity-60")}>
      {on ? <RepeatIcon className={iconCls} /> : <Repeat1Icon className={iconCls} />}
    </IconButton>
  )
}

export function PotPlayerButton({ onClick }: { onClick: () => void }) {
  return (
    <IconButton label="팟플레이어로 열기" onClick={onClick}>
      <MonitorPlayIcon className={iconCls} />
    </IconButton>
  )
}

export function WebFullscreenButton({ on, onToggle }: { on: boolean; onToggle: () => void }) {
  return (
    <IconButton label={on ? "브라우저 전체 화면 끝내기" : "브라우저 화면에 꽉 차게"} onClick={onToggle} pressed={on}>
      {on ? <Minimize2Icon className={iconCls} /> : <Maximize2Icon className={iconCls} />}
    </IconButton>
  )
}

const itemCls = cn(
  "group/menu-trigger-item",
  "relative flex cursor-pointer select-none items-center gap-1.5 rounded-media-menu-item px-2 py-1.5 text-start whitespace-nowrap",
  "focus-ring-media media-highlighted:highlight-media focus-visible:outline-media-ring focus-visible:outline-offset-2",
  "text-shadow-media transition-[background-color,color] duration-media-fast ease-in-out",
  "media-anchored:duration-media-instant media-anchored:media-highlighted:duration-media-slow",
  "justify-between tabular-nums text-inherit",
  "media-anchored:data-highlighted:[anchor-name:--media-menu-item-highlight-anchor] media-anchored:media-highlighted:bg-transparent",
)

const contentCls = cn(
  "[anchor-scope:--media-menu-item-highlight-anchor] media-anchored:before:anchor-media-highlight",
  "media-transitioning:before:hidden media-transitioning:[&_*]:before:hidden media-anchored:has-data-[highlighted=]:before:duration-0",
  "data-submenu:media-transitioning:pointer-events-none data-submenu:media-transitioning:overflow-hidden data-submenu:media-transitioning:blur-media-hidden",
  "absolute max-h-[inherit] overflow-auto overscroll-none outline-hidden transition-[translate,filter] duration-media-menu ease-out",
  "data-submenu:inset-x-0 data-submenu:top-0 data-submenu:z-10 data-submenu:max-h-[inherit] data-submenu:p-1",
  "data-submenu:media-transitioning:translate-x-full data-submenu:media-transitioning:rtl:-translate-x-full",
)

export interface Choice {
  value: string
  label: string
  hint?: string
}

/** A settings submenu with radio choices, styled like the skin's built-in submenus. */
export function ChoiceSubmenu({ icon, title, value, choices, onChange }: { icon: ReactNode; title: string; value: string; choices: Choice[]; onChange: (v: string) => void }) {
  const current = choices.find((c) => c.value === value)?.label ?? ""
  return (
    <Menu.Root>
      <Menu.Trigger className={itemCls}>
        <span className="shrink-0 text-media-muted-foreground group-media-highlighted/menu-trigger-item:text-inherit [&_svg]:size-media-icon">{icon}</span>
        <TextPrimitive>{title}</TextPrimitive>
        <span className="ms-auto inline-flex min-w-0 items-center gap-1 ps-2">
          <span className="max-w-28 truncate opacity-65">{current}</span>
          <MenuChevron />
        </span>
      </Menu.Trigger>
      <Menu.Content className={contentCls}>
        <Menu.Item className={cn(itemCls, "mb-0.5 w-full justify-start")}>
          <MenuChevron back />
          <TextPrimitive>{title}</TextPrimitive>
        </Menu.Item>
        <Menu.Separator className="my-1 block border-b border-media-border shadow-media-separator" />
        <Menu.RadioGroup value={value} onValueChange={(v) => onChange(String(v))} className="flex flex-col gap-0.5" aria-label={title}>
          {choices.map((c) => (
            <RadioItem key={c.value} value={c.value}>
              <span>
                {c.label}
                {c.hint ? <span className="ps-1.5 text-media-xs opacity-60">{c.hint}</span> : null}
              </span>
            </RadioItem>
          ))}
        </Menu.RadioGroup>
      </Menu.Content>
    </Menu.Root>
  )
}
