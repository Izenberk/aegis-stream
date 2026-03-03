import { LineChart, Line, XAxis, YAxis, Tooltip, Legend, ResponsiveContainer } from 'recharts';
import type { MetricsSnapshot } from '../types/api';

const COLORS = ['#3b82f6', '#f59e0b', '#10b981', '#ef4444', '#8b5cf6'];

export function EventsChart({ history }: { history: MetricsSnapshot[] }) {
  // Collect all event types seen across history.
  const types = new Set<string>();
  history.forEach((s) => Object.keys(s.eventsPerSec).forEach((t) => types.add(t)));
  const typeList = Array.from(types);

  const data = history.map((s) => {
    const point: Record<string, number> = {
      time: s.timestamp,
    };
    typeList.forEach((t) => {
      point[t] = s.eventsPerSec[t] ?? 0;
    });
    // Add total for tooltip convenience.
    point['total'] = Object.values(s.eventsPerSec).reduce((a, b) => a + b, 0);
    return point;
  });

  return (
    <div className="bg-gray-800 rounded-lg p-4 border border-gray-700">
      <h3 className="text-sm font-medium text-gray-300 mb-3">Events / sec</h3>
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
            formatter={(val: number) => val.toFixed(1)}
          />
          <Legend />
          {typeList.map((t, i) => (
            <Line
              key={t}
              type="monotone"
              dataKey={t}
              stroke={COLORS[i % COLORS.length]}
              dot={false}
              strokeWidth={2}
            />
          ))}
        </LineChart>
      </ResponsiveContainer>
    </div>
  );
}
