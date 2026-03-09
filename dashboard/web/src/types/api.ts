export interface Metrics {
  eventsPerSec: Record<string, number>;
  queueDepth: number;
  activeConnections: number;
  errorRate: number;
  latencyP50: number;
  latencyP95: number;
  podCount: number;
  totalEvents: number;
}

export interface PipelineSpec {
  replicas: number;
  workers: number;
  queueDepth: number;
  port: number;
  metricsPort: number;
  costPerPodHour: string;
  image: string;
}

export interface PipelineStatus {
  readyReplicas: number;
  phase: string;
}

export interface PipelineInfo {
  name: string;
  spec: PipelineSpec;
  status: PipelineStatus;
}

export interface MetricsSnapshot extends Metrics {
  timestamp: number;
}

export interface Trade {
  eventId: string;
  symbol: string;
  price: number;
  volume: number;
  eventTs: number;
  createdAt: string;
}
