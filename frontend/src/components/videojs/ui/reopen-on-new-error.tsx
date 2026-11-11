'use client';

import { selectError, useDialogContext, usePlayer } from '@videojs/react';
import { useEffect } from 'react';

// @videojs/react opens this controlled dialog from an effect that compares the `open` prop with the
// transition's `active` flag, which a close transition keeps true until its exit animation ends. An
// error that arrives while the dialog is closing therefore never reaches dialog.open(), and the
// close finishes with the error still in the store. Opening per new error takes the dialog's own
// reopen path, which cancels the running close and restarts the open one, and is a no-op while the
// dialog is already open and idle.
export function ReopenOnNewError() {
  const { dialog } = useDialogContext();
  const error = usePlayer(selectError)?.error;
  useEffect(() => {
    if (error) dialog.open();
  }, [dialog, error]);
  return null;
}
