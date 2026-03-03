import { useState, useEffect, useRef, useCallback } from 'react';
import type { Metrics, MetricsSnapshot } from '../types/api';

const POLL_INTERVAL = 5000;
const HISTORY_SIZE = 24; // 2 minutes at 5s intervals

export function useMetrics() {
  const [current, setCurrent] = useState<Metrics | null>(null);
  const [history, setHistory] = useState<MetricsSnapshot[]>([]);
  const [error, setError] = useState<string | null>(null);
  const historyRef = useRef<MetricsSnapshot[]>([]);

  const fetchMetrics = useCallback(async () => {
    try {
      const res = await fetch('/api/metrics');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: Metrics = await res.json();
      setCurrent(data);
      setError(null);

      const snapshot: MetricsSnapshot = { ...data, timestamp: Date.now() };
      const next = [...historyRef.current, snapshot].slice(-HISTORY_SIZE);
      historyRef.current = next;
      setHistory(next);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error');
    }
  }, []);

  useEffect(() => {
    fetchMetrics();
    const id = setInterval(fetchMetrics, POLL_INTERVAL);
    return () => clearInterval(id);
  }, [fetchMetrics]);

  return { current, history, error };
}
