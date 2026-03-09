import type { Trade } from '../types/api';

// TradesTable shows the most recent trades stored in PostgreSQL.
// Each row shows symbol, price, volume, and when it was stored.
// The table updates every 5 seconds via the useTrades hook.

interface Props {
  trades: Trade[];
}

export function TradesTable({ trades }: Props) {
  if (trades.length === 0) {
    return (
      <div className="bg-gray-800 rounded-xl p-6 border border-gray-700">
        <h3 className="text-sm font-medium text-gray-400 mb-4">Recent Trades</h3>
        <p className="text-gray-500 text-sm">
          No trades yet. Start the server with{' '}
          <code className="text-gray-400">-sink postgres</code> and run the feed.
        </p>
      </div>
    );
  }

  return (
    <div className="bg-gray-800 rounded-xl p-6 border border-gray-700">
      <h3 className="text-sm font-medium text-gray-400 mb-4">
        Recent Trades
        <span className="ml-2 text-xs text-gray-500">
          (last {trades.length} from PostgreSQL)
        </span>
      </h3>
      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="text-gray-400 border-b border-gray-700">
              <th className="text-left py-2 pr-4">Symbol</th>
              <th className="text-right py-2 pr-4">Price</th>
              <th className="text-right py-2 pr-4">Volume</th>
              <th className="text-right py-2">Time</th>
            </tr>
          </thead>
          <tbody>
            {trades.map((trade, i) => (
              <tr
                key={trade.eventId || i}
                className="border-b border-gray-700/50 hover:bg-gray-700/30"
              >
                <td className="py-2 pr-4 font-mono text-white">
                  {trade.symbol}
                </td>
                <td className="py-2 pr-4 text-right font-mono text-green-400">
                  {formatPrice(trade.price)}
                </td>
                <td className="py-2 pr-4 text-right font-mono text-gray-300">
                  {trade.volume.toLocaleString()}
                </td>
                <td className="py-2 text-right text-gray-400">
                  {formatTime(trade.createdAt)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// Format price with appropriate decimal places based on magnitude.
// BTC ~90,000 → 2 decimals; SOL ~90 → 2 decimals; sub-dollar → 4 decimals.
function formatPrice(price: number): string {
  if (price >= 1) {
    return `$${price.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
  }
  return `$${price.toFixed(4)}`;
}

// Format ISO timestamp to a short local time string.
function formatTime(isoString: string): string {
  const date = new Date(isoString);
  return date.toLocaleTimeString();
}
