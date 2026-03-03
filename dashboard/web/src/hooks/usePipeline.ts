import { useState, useEffect, useCallback } from 'react';
import type { PipelineInfo } from '../types/api';

const POLL_INTERVAL = 10000;

export function usePipeline() {
  const [pipeline, setPipeline] = useState<PipelineInfo | null>(null);
  const [error, setError] = useState<string | null>(null);

  const fetchPipeline = useCallback(async () => {
    try {
      const res = await fetch('/api/pipeline');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: PipelineInfo = await res.json();
      setPipeline(data);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error');
    }
  }, []);

  useEffect(() => {
    fetchPipeline();
    const id = setInterval(fetchPipeline, POLL_INTERVAL);
    return () => clearInterval(id);
  }, [fetchPipeline]);

  return { pipeline, error };
}
