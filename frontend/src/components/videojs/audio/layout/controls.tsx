import { Controls, Time, Tooltip } from '@videojs/react';

import { ButtonTooltip } from '@/components/videojs/ui/button-tooltip';
import { SeekButton } from '@/components/videojs/ui/seek-button';
import { VolumePopover } from '@/components/videojs/ui/volume-popover';
import { cn } from '@/lib/resolve-class-name';

import { AudioPlayButton } from '../buttons/play-button';
import { AudioSettingsMenu } from '../menus/settings-menu';
import { AudioTimeSlider } from '../sliders/time-slider';
import { usePlayerSlots } from '../../slots';

export function DefaultAudioControls() {
  const slots = usePlayerSlots();
  return (
    <Controls.Root visibility="always">
      <Controls.Content
        className={cn(
          'audio-controls',
          'relative z-20 flex items-center rtl:flex-row-reverse rounded-media-controls bg-media-controls text-media-controls-foreground text-shadow-media',
          'p-0.5 media-lg:p-1 surface-media after:surface-media-inset'
        )}
      >
        <Tooltip.Provider>
          <Controls.Group className={'flex items-center rtl:flex-row-reverse gap-px'}>
            {slots.beforePlay}
            <AudioPlayButton />
            {slots.afterPlay}
            <ButtonTooltip boundary="viewport" side="top">
              <SeekButton className={'media-max-lg:hidden'} seconds={-10} />
            </ButtonTooltip>
            <ButtonTooltip boundary="viewport" side="top">
              <SeekButton className={'media-max-lg:hidden'} seconds={10} />
            </ButtonTooltip>
          </Controls.Group>

          <Controls.Group
            className={
              '@container/audio-time-controls flex min-w-0 flex-1 items-center rtl:flex-row-reverse gap-2.5 px-3'
            }
          >
            <Time.Value
              className={'tabular-nums transition-opacity duration-media-slow ease-out data-unavailable:opacity-50'}
              type="current"
            />
            <AudioTimeSlider previewOverflow="visible" />
            <Time.Value
              className={cn(
                'cursor-pointer rounded-sm tabular-nums focus-ring-media',
                'aria-disabled:cursor-not-allowed aria-disabled:opacity-50',
                'transition-[outline-color,outline-offset] duration-media-fast ease-out',
                'focus-visible:outline-media-ring focus-visible:outline-offset-2',
                '@max-[16rem]/audio-time-controls:hidden'
              )}
              type="remaining"
              toggle
            />
          </Controls.Group>

          <Controls.Group className={'flex items-center rtl:flex-row-reverse gap-px'}>
            {slots.end}
            <AudioSettingsMenu />
            <VolumePopover boundary="viewport" />
          </Controls.Group>
        </Tooltip.Provider>
      </Controls.Content>
    </Controls.Root>
  );
}
