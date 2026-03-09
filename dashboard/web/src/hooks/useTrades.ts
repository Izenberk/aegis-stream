import { useState, useEffect, useCallback } from 'react';
import type { Trade } from '../types/api';

// Poll /api/trades every 5 seconds to show the latest trades from PostgreSQL.
const POLL_INTERVAL = 5_000;

export function useTrades() {
  const [trades, setTrades] = useState<Trade[]>([]);
  const [error, setError] = useState<string | null>(null);

  const fetchTrades = useCallback(async () => {
    try {
      const res = await fetch('/api/trades');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: Trade[] = await res.json();
      setTrades(data);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'fetch failed');
    }
  }, []);

  useEffect(() => {
    fetchTrades();
    const id = setInterval(fetchTrades, POLL_INTERVAL);
    return () => clearInterval(id);
  }, [fetchTrades]);

  return { trades, error };
}
