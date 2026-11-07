import type { ComponentProps } from 'react';

import { AudioTrackMenu } from '@/components/videojs/ui/audio-track-menu';
import { CaptionsSubmenu } from '@/components/videojs/ui/captions-submenu';
import { PlaybackRateSubmenu } from '@/components/videojs/ui/playback-rate-submenu';
import { QualityMenu } from '@/components/videojs/ui/quality-menu';
import { SettingsMenu } from '@/components/videojs/ui/settings-menu';
import { usePlayerSlots } from '../../slots';

export type VideoSettingsMenuProps = Omit<NonNullable<ComponentProps<typeof SettingsMenu>>, 'children'>;

export function VideoSettingsMenu(props: VideoSettingsMenuProps = {}) {
  const slots = usePlayerSlots();
  return (
    <SettingsMenu {...props}>
      {slots.settings}
      <QualityMenu />
      <AudioTrackMenu />
      <PlaybackRateSubmenu />
      <CaptionsSubmenu />
    </SettingsMenu>
  );
}
