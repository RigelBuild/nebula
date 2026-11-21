import { useRef, useState } from "react";

/** A bounded row cursor that starts at `start`. Tail-following lists start at the end and stay there
 *  until the user moves away. */
export function useListCursor(length: number, { followTail = false, start = 0 }: { followTail?: boolean; start?: number } = {}) {
  // Ink binds key handlers after paint, so a press can run an older render's handler.
  // Read the length at press time instead of the one that handler captured.
  const latestLength = useRef(length);
  latestLength.current = length;
  const lastIndex = Math.max(0, length - 1);
  const clamp = (index: number) => Math.max(0, Math.min(index, lastIndex));
  const [cursor, setCursor] = useState(() => ({ length, index: followTail ? lastIndex : clamp(start) }));
  let index = clamp(cursor.index);

  // Adjust before committing children or input handlers, so a shrink cannot leave hidden state
  // beyond the visible selection or race an effect against a key press.
  if (cursor.length !== length) {
    if (followTail && cursor.index >= cursor.length - 1) index = lastIndex;
    setCursor({ length, index });
  }

  const move = (delta: number) => setCursor((previous) => {
    const last = Math.max(0, latestLength.current - 1);
    return { length: latestLength.current, index: Math.max(0, Math.min(previous.index + delta, last)) };
  });
  return {
    index,
    up: () => move(-1),
    down: () => move(1),
    first: () => setCursor({ length: latestLength.current, index: 0 }),
    last: () => setCursor({ length: latestLength.current, index: Math.max(0, latestLength.current - 1) }),
  };
}
