import { useMetrics } from './hooks/useMetrics';
import { usePipeline } from './hooks/usePipeline';
import { useTrades } from './hooks/useTrades';
import { Header } from './components/Header';
import { MetricsCard } from './components/MetricsCard';
import { EventsChart } from './components/EventsChart';
import { QueueChart } from './components/QueueChart';
import { LatencyChart } from './components/LatencyChart';
import { CostPanel } from './components/CostPanel';
import { TradesTable } from './components/TradesTable';

function App() {
  const { current, history, error: metricsError } = useMetrics();
  const { pipeline, error: pipelineError } = usePipeline();
  const { trades } = useTrades();

  const totalEventsPerSec = current
    ? Object.values(current.eventsPerSec).reduce((a, b) => a + b, 0)
    : 0;

  return (
    <div className="min-h-screen bg-gray-900 text-white">
      <Header pipeline={pipeline} />

      {(metricsError || pipelineError) && (
        <div className="mx-6 mt-4 p-3 bg-red-900/50 border border-red-700 rounded-lg text-sm text-red-300">
          {metricsError && <div>Metrics: {metricsError}</div>}
          {pipelineError && <div>Pipeline: {pipelineError}</div>}
        </div>
      )}

      <main className="p-6 space-y-6">
        {/* Stat cards row */}
        <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
          <MetricsCard
            label="Pods"
            value={
              pipeline
                ? `${pipeline.status.readyReplicas}/${pipeline.spec.replicas}`
                : '—'
            }
          />
          <MetricsCard
            label="Events/sec"
            value={current ? totalEventsPerSec.toLocaleString(undefined, { maximumFractionDigits: 0 }) : '—'}
          />
          <MetricsCard
            label="Queue Depth"
            value={current ? current.queueDepth.toLocaleString() : '—'}
          />
          <MetricsCard
            label="Connections"
            value={current ? current.activeConnections.toLocaleString() : '—'}
          />
        </div>

        {/* Events chart — full width */}
        <EventsChart history={history} />

        {/* Queue + Latency side by side */}
        <div className="grid grid-cols-1 md:grid-cols-2 gap-6">
          <QueueChart history={history} />
          <LatencyChart history={history} />
        </div>

        {/* Recent trades from PostgreSQL */}
        <TradesTable trades={trades} />

        {/* Cost panel */}
        <CostPanel pipeline={pipeline} />
      </main>
    </div>
  );
}

export default App;
