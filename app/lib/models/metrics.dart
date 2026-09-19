import 'json.dart';

/// `/api/v1/metrics/series` 的数据点。step=raw 时 avg=value，min/max 同值。
class MetricPoint {
  const MetricPoint({
    required this.ts,
    required this.avg,
    this.min,
    this.max,
  });

  final DateTime ts;
  final double avg;
  final double? min;
  final double? max;

  factory MetricPoint.fromJson(Map<String, dynamic> json) => MetricPoint(
        ts: asDate(json['ts']),
        avg: asDoubleOrNull(json['avg']) ?? 0,
        min: asDoubleOrNull(json['min']),
        max: asDoubleOrNull(json['max']),
      );
}

/// 趋势接口支持的指标种类。
enum MetricKind {
  cpuPercent('cpu_percent', 'CPU', '%'),
  memPercent('mem_percent', '内存', '%'),
  diskPercent('disk_percent', '磁盘', '%'),
  netRxBps('net_rx_bps', '网络下行', 'B/s'),
  netTxBps('net_tx_bps', '网络上行', 'B/s');

  const MetricKind(this.apiValue, this.label, this.unit);

  final String apiValue;
  final String label;
  final String unit;
}
