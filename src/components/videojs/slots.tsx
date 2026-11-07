import { createContext, useContext, type ReactNode } from 'react';

/**
 * App-specific controls injected into the installed skin source without forking every
 * file: previous/next, auto-next, PotPlayer, web fullscreen, and extra settings submenus.
 */
export interface PlayerSlots {
  beforePlay?: ReactNode;
  afterPlay?: ReactNode;
  /** Bottom bar, right side, before the fullscreen button. */
  end?: ReactNode;
  /** Extra submenus at the top of the settings menu. */
  settings?: ReactNode;
  /** Layers inside the player container, under the controls (e.g. the VR canvas). */
  layers?: ReactNode;
  retryPlayback?: () => void;
}

export const PlayerSlotsContext = createContext<PlayerSlots>({});

export const usePlayerSlots = () => useContext(PlayerSlotsContext);
