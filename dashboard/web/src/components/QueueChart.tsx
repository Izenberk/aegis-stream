import { AreaChart, Area, XAxis, YAxis, Tooltip, ResponsiveContainer } from 'recharts';
import type { MetricsSnapshot } from '../types/api';

export function QueueChart({ history }: { history: MetricsSnapshot[] }) {
  const data = history.map((s) => ({
    time: s.timestamp,
    depth: s.queueDepth,
  }));

  return (
    <div className="bg-gray-800 rounded-lg p-4 border border-gray-700">
      <h3 className="text-sm font-medium text-gray-300 mb-3">Queue Depth</h3>
      <ResponsiveContainer width="100%" height={200}>
        <AreaChart data={data}>
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
            formatter={(val: number) => val.toLocaleString()}
          />
          <Area
            type="monotone"
            dataKey="depth"
            stroke="#f59e0b"
            fill="#f59e0b"
            fillOpacity={0.15}
            strokeWidth={2}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}
