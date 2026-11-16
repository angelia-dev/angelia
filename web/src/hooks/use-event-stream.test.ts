import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { eventRecord } from "@/test/fixtures";
import { MockEventSource } from "@/test/mock-api";
import { MAX_LIVE_EVENTS, useEventStream } from "./use-event-stream";

beforeEach(() => {
  MockEventSource.reset();
  vi.stubGlobal("EventSource", MockEventSource);
});
afterEach(() => vi.unstubAllGlobals());

describe("useEventStream", () => {
  it("keeps only the newest live events in memory", () => {
    const { result } = renderHook(() =>
      useEventStream({ filters: {}, after: 0, enabled: true }),
    );
    const es = MockEventSource.last as MockEventSource;
    act(() => es.open());
    act(() => {
      for (let seq = 1; seq <= MAX_LIVE_EVENTS + 10; seq++)
        es.emit(eventRecord(seq));
    });
    expect(result.current.events).toHaveLength(MAX_LIVE_EVENTS);
    expect(result.current.events[0]?.seq).toBe(11);
    expect(result.current.events.at(-1)?.seq).toBe(MAX_LIVE_EVENTS + 10);
  });
});
