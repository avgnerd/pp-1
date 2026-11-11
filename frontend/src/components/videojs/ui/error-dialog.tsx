'use client';

import '../styles/base.css';
import '../styles/audio/theme.css';
import '../styles/video/captions.css';
import '../styles/video/theme.css';
import { ErrorDialog as ErrorDialogPrimitive, selectError, useDialogContext, usePlayer } from '@videojs/react';
import { useEffect } from 'react';

import { Button } from '@/components/videojs/ui/button';
import { usePlayerSlots } from '@/components/videojs/slots';
import { cn } from '@/lib/resolve-class-name';

// @videojs/react opens this controlled dialog from an effect that compares the `open` prop with the
// transition's `active` flag, which a close transition keeps true until its exit animation ends. An
// error that arrives while the dialog is closing therefore never reaches dialog.open(), and the
// close finishes with the error still in the store. Opening per new error takes the dialog's own
// reopen path, which cancels the running close and restarts the open one, and is a no-op while the
// dialog is already open and idle.
function ReopenOnNewError() {
  const { dialog } = useDialogContext();
  const error = usePlayer(selectError)?.error;
  useEffect(() => {
    if (error) dialog.open();
  }, [dialog, error]);
  return null;
}

export function ErrorDialog() {
  const { retryPlayback } = usePlayerSlots();
  return (
    <ErrorDialogPrimitive.Root>
      <ReopenOnNewError />
      <ErrorDialogPrimitive.Backdrop
        className={cn(
          'absolute inset-0 z-40 bg-media-backdrop/20 opacity-100 backdrop-filter-media-dialog rounded-[inherit]',
          'not-data-open:hidden transition-opacity delay-media-dialog duration-media-dialog ease-out',
          'media-transitioning:opacity-0 data-ending-style:delay-0',
          'data-ending-style:duration-media-slower'
        )}
      />
      <ErrorDialogPrimitive.Popup
        className={cn(
          'absolute top-1/2 left-1/2 z-50 flex w-media-dialog-width max-w-media-dialog max-h-[calc(100%-0.5rem)] -translate-x-1/2 -translate-y-1/2 flex-col gap-3 rounded-media-dialog text-media-popover-foreground outline-hidden not-data-open:hidden',
          'text-shadow-media-dialog transition-[opacity,scale] delay-media-dialog duration-media-dialog ease-out',
          'media-transitioning:scale-media-hidden-popup media-transitioning:opacity-0 data-ending-style:delay-0',
          'bg-media-popover surface-media after:surface-media-inset',
          'p-3',
          'data-ending-style:duration-media-slower'
        )}
      >
        <div className={cn('flex min-h-0 flex-col gap-2 overflow-y-auto', 'px-2 pt-2 pb-1.5')}>
          <ErrorDialogPrimitive.Title className={'m-0 text-media-lg font-semibold leading-tight'} />
          <ErrorDialogPrimitive.Description className={'m-0 opacity-70 wrap-anywhere'} />
        </div>
        <div className={'flex shrink-0 gap-2'}>
          <ErrorDialogPrimitive.Close
            onClick={retryPlayback}
            render={<Button />}
            className={
              'h-media-control w-full flex-1 bg-media-primary! px-4 py-2 font-medium text-media-primary-foreground!'
            }
          >
            {retryPlayback ? '다시 재생' : undefined}
          </ErrorDialogPrimitive.Close>
        </div>
      </ErrorDialogPrimitive.Popup>
    </ErrorDialogPrimitive.Root>
  );
}
