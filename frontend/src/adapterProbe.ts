export type ModelAdapter = {
  displayName: string;
  type: string;
  baseURL: string;
  apiKey: string;
  modelID: string;
  contextWindow?: string;
  supportedThinkingLevels?: string[];
  serviceTier?: string;
  maxOutputTokens?: string;
  thinkingBudget?: string;
  notes?: string;
  lastTestResult?: string;
  lastTestedAt?: number;
};

// Ignore result metadata and Go's omitted empty optional fields when comparing
// a probe with the adapter configuration that the user actually tested.
export function adapterProbeFingerprint(adapter: ModelAdapter): string {
  return JSON.stringify(Object.entries(adapter)
    .filter(([key, value]) => key !== "lastTestResult" && key !== "lastTestedAt"
      && value !== undefined && value !== null && value !== ""
      && !(Array.isArray(value) && value.length === 0))
    .sort(([a], [b]) => a.localeCompare(b)));
}

export function captureAdapterProbe(row: ModelAdapter) {
  return { row, fingerprint: adapterProbeFingerprint(row) };
}

export function applyAdapterProbeResult(
  current: ModelAdapter | undefined,
  probe: ReturnType<typeof captureAdapterProbe>,
  result: ModelAdapter,
): boolean {
  if (current !== probe.row || !current
    || adapterProbeFingerprint(current) !== probe.fingerprint
    || adapterProbeFingerprint(result) !== probe.fingerprint) return false;
  // Never replace the row with a stale server snapshot. Merge test metadata only.
  current.lastTestResult = result.lastTestResult;
  current.lastTestedAt = result.lastTestedAt;
  return true;
}
