import { LineChart, Line, XAxis, YAxis, Tooltip, Legend, ResponsiveContainer } from 'recharts';
import type { MetricsSnapshot } from '../types/api';

export function LatencyChart({ history }: { history: MetricsSnapshot[] }) {
  const data = history.map((s) => ({
    time: s.timestamp,
    p50: s.latencyP50 * 1000, // Convert seconds to milliseconds for readability.
    p95: s.latencyP95 * 1000,
  }));

  return (
    <div className="bg-gray-800 rounded-lg p-4 border border-gray-700">
      <h3 className="text-sm font-medium text-gray-300 mb-3">Latency (ms)</h3>
      <ResponsiveContainer width="100%" height={200}>
        <LineChart data={data}>
          <XAxis
            dataKey="time"
            tickFormatter={(t) => new Date(t).toLocaleTimeString()}
            stroke="#6b7280"
            fontSize={11}
          />
          <YAxis stroke="#6b7280" fontSize={11} />
          <Tooltip
            contentStyle={{ backgroundColor: '#1f2937', border: 'none', borderRadius: 8 }}
            labelFormatter={(t) => new Date(t as number).toLocaleTimeString()}
            formatter={(val: number) => `${val.toFixed(2)} ms`}
          />
          <Legend />
          <Line type="monotone" dataKey="p50" stroke="#10b981" dot={false} strokeWidth={2} />
          <Line type="monotone" dataKey="p95" stroke="#ef4444" dot={false} strokeWidth={2} />
        </LineChart>
      </ResponsiveContainer>
    </div>
  );
}
