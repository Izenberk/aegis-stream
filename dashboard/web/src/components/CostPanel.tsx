import type { PipelineInfo } from '../types/api';

interface CostPanelProps {
  pipeline: PipelineInfo | null;
}

export function CostPanel({ pipeline }: CostPanelProps) {
  if (!pipeline) return null;

  const costPerPodHour = parseFloat(pipeline.spec.costPerPodHour) || 0;
  const replicas = pipeline.status.readyReplicas;
  const perSec = (costPerPodHour * replicas) / 3600;
  const perHour = costPerPodHour * replicas;
  const perDay = perHour * 24;
  const perMonth = perDay * 30;

  return (
    <div className="bg-gray-800 rounded-lg p-4 border border-gray-700">
      <h3 className="text-sm font-medium text-gray-300 mb-3">Estimated Cost</h3>
      <div className="grid grid-cols-4 gap-4 text-center">
        <CostItem label="/sec" value={perSec} precision={6} />
        <CostItem label="/hr" value={perHour} precision={2} />
        <CostItem label="/day" value={perDay} precision={2} />
        <CostItem label="/mo" value={perMonth} precision={0} />
      </div>
      <div className="text-xs text-gray-500 mt-3">
        Based on ${costPerPodHour}/pod/hr x {replicas} pod{replicas !== 1 ? 's' : ''}
      </div>
    </div>
  );
}

function CostItem({ label, value, precision }: { label: string; value: number; precision: number }) {
  return (
    <div>
      <div className="text-lg font-bold text-green-400">${value.toFixed(precision)}</div>
      <div className="text-xs text-gray-400">{label}</div>
    </div>
  );
}
