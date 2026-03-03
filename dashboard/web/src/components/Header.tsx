import type { PipelineInfo } from '../types/api';

const phaseColors: Record<string, string> = {
  Running: 'bg-green-500',
  Pending: 'bg-yellow-500',
  Failed: 'bg-red-500',
};

export function Header({ pipeline }: { pipeline: PipelineInfo | null }) {
  const phase = pipeline?.status.phase ?? 'Unknown';
  const dotColor = phaseColors[phase] ?? 'bg-gray-500';

  return (
    <header className="flex items-center justify-between px-6 py-4 bg-gray-900 border-b border-gray-700">
      <h1 className="text-xl font-bold text-white">
        Aegis Stream Dashboard
      </h1>
      <div className="flex items-center gap-2 text-sm text-gray-300">
        <span className={`inline-block w-2.5 h-2.5 rounded-full ${dotColor}`} />
        {phase}
      </div>
    </header>
  );
}
