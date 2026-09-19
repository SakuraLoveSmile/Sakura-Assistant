import 'package:fl_chart/fl_chart.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../models/metrics.dart';
import '../state/data_providers.dart';
import '../state/providers.dart';
import '../widgets/common.dart';
import '../widgets/format.dart';

/// 趋势页：cpu/mem/disk/net 折线图，raw/5m 切换。
/// raw=最近 6 小时原始样本；5m=最近 7 天五分钟汇总。
class TrendsPage extends ConsumerStatefulWidget {
  const TrendsPage({super.key, required this.sourceId, this.sourceName});

  final String sourceId;
  final String? sourceName;

  @override
  ConsumerState<TrendsPage> createState() => _TrendsPageState();
}

class _TrendsPageState extends ConsumerState<TrendsPage> {
  /// cpu | mem | disk | net（net 显示 rx+tx 两条线）。
  String _group = 'cpu';
  String _step = 'raw';
  String? _diskMount; // disk_percent 必填 label

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: Text('${widget.sourceName ?? '设备'} · 趋势')),
      body: ListView(
        padding: const EdgeInsets.all(16),
        children: [
          SegmentedButton<String>(
            segments: const [
              ButtonSegment(value: 'cpu', label: Text('CPU')),
              ButtonSegment(value: 'mem', label: Text('内存')),
              ButtonSegment(value: 'disk', label: Text('磁盘')),
              ButtonSegment(value: 'net', label: Text('网络')),
            ],
            selected: {_group},
            onSelectionChanged: (s) => setState(() => _group = s.first),
          ),
          const SizedBox(height: 12),
          Row(
            children: [
              SegmentedButton<String>(
                segments: const [
                  ButtonSegment(value: 'raw', label: Text('原始')),
                  ButtonSegment(value: '5m', label: Text('5 分钟')),
                ],
                selected: {_step},
                onSelectionChanged: (s) => setState(() => _step = s.first),
                style: const ButtonStyle(visualDensity: VisualDensity.compact),
              ),
              const SizedBox(width: 12),
              Text(
                _step == 'raw' ? '最近 6 小时' : '最近 7 天',
                style: Theme.of(context)
                    .textTheme
                    .bodySmall
                    ?.copyWith(color: Theme.of(context).colorScheme.outline),
              ),
            ],
          ),
          const SizedBox(height: 16),
          if (_group == 'disk') _MountPicker(
            sourceId: widget.sourceId,
            value: _diskMount,
            onChanged: (v) => setState(() => _diskMount = v),
          ),
          SizedBox(height: 320, child: _chart()),
          const SizedBox(height: 12),
          Text(
            '区间缺数据处曲线断档（不补 0）。原始样本保留 7 天，5 分钟汇总保留 90 天。',
            style: Theme.of(context)
                .textTheme
                .bodySmall
                ?.copyWith(color: Theme.of(context).colorScheme.outline),
          ),
        ],
      ),
    );
  }

  Widget _chart() {
    switch (_group) {
      case 'mem':
        return _MetricChart(
          query: TrendsQuery(
              sourceId: widget.sourceId, metric: 'mem_percent', step: _step),
          title: '内存占用 %',
          unit: '%',
        );
      case 'disk':
        final mount = _diskMount ?? '/';
        return _MetricChart(
          query: TrendsQuery(
            sourceId: widget.sourceId,
            metric: 'disk_percent',
            step: _step,
            label: 'mount=$mount',
          ),
          title: '磁盘占用 %（$mount）',
          unit: '%',
        );
      case 'net':
        return _NetChart(sourceId: widget.sourceId, step: _step);
      case 'cpu':
      default:
        return _MetricChart(
          query: TrendsQuery(
              sourceId: widget.sourceId, metric: 'cpu_percent', step: _step),
          title: 'CPU 占用 %',
          unit: '%',
        );
    }
  }
}

class _MountPicker extends ConsumerWidget {
  const _MountPicker({
    required this.sourceId,
    required this.value,
    required this.onChanged,
  });

  final String sourceId;
  final String? value;
  final ValueChanged<String> onChanged;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    // 挂载点选项来自缓存/概览里该来源最近样本的 disks。
    return FutureBuilder(
      future: ref.read(cacheProvider).sourceById(sourceId),
      builder: (context, snap) {
        final disks = snap.data?.summary?.disks ?? const [];
        final mounts = disks.map((d) => d.mount).toSet().toList();
        if (mounts.isEmpty) mounts.add('/');
        final current = mounts.contains(value) ? value! : mounts.first;
        return Padding(
          padding: const EdgeInsets.only(bottom: 12),
          child: Wrap(
            spacing: 8,
            children: [
              for (final m in mounts)
                ChoiceChip(
                  label: Text(m),
                  selected: m == current,
                  onSelected: (_) => onChanged(m),
                ),
            ],
          ),
        );
      },
    );
  }
}

class _MetricChart extends ConsumerWidget {
  const _MetricChart({
    required this.query,
    required this.title,
    required this.unit,
  });

  final TrendsQuery query;
  final String title;
  final String unit;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final points = ref.watch(trendsProvider(query));
    return AsyncValueView(
      value: points,
      onRetry: () => ref.invalidate(trendsProvider(query)),
      builder: (data) => _LineChart(
        title: title,
        unit: unit,
        series: [_ChartSeries(name: title, points: data)],
      ),
    );
  }
}

class _NetChart extends ConsumerWidget {
  const _NetChart({required this.sourceId, required this.step});

  final String sourceId;
  final String step;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final rx = ref.watch(trendsProvider(TrendsQuery(
        sourceId: sourceId, metric: 'net_rx_bps', step: step)));
    final tx = ref.watch(trendsProvider(TrendsQuery(
        sourceId: sourceId, metric: 'net_tx_bps', step: step)));
    if (rx.hasError) {
      return ErrorView(
          error: rx.error!,
          onRetry: () => ref.invalidate(trendsProvider));
    }
    if (tx.hasError) {
      return ErrorView(
          error: tx.error!,
          onRetry: () => ref.invalidate(trendsProvider));
    }
    if (!rx.hasValue || !tx.hasValue) {
      return const Center(child: CircularProgressIndicator());
    }
    return _LineChart(
      title: '网络速率',
      unit: 'B/s',
      series: [
        _ChartSeries(name: '下行', points: rx.value!),
        _ChartSeries(name: '上行', points: tx.value!),
      ],
      yFormatter: formatBps,
    );
  }
}

class _ChartSeries {
  _ChartSeries({required this.name, required this.points});

  final String name;
  final List<MetricPoint> points;
}

class _LineChart extends StatelessWidget {
  const _LineChart({
    required this.title,
    required this.unit,
    required this.series,
    this.yFormatter,
  });

  final String title;
  final String unit;
  final List<_ChartSeries> series;
  final String Function(double?)? yFormatter;

  static const _colors = [Color(0xFF3D6FE0), Color(0xFF2E9E5B), Color(0xFFE8960C)];

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final scheme = theme.colorScheme;
    final allEmpty = series.every((s) => s.points.isEmpty);
    return Card(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(12, 16, 16, 8),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                Text(title, style: theme.textTheme.titleSmall),
                const Spacer(),
                for (var i = 0; i < series.length; i++)
                  Padding(
                    padding: const EdgeInsets.only(left: 12),
                    child: Row(
                      mainAxisSize: MainAxisSize.min,
                      children: [
                        Container(
                          width: 12,
                          height: 3,
                          color: _colors[i % _colors.length],
                        ),
                        const SizedBox(width: 4),
                        Text(series[i].name,
                            style: theme.textTheme.labelSmall),
                      ],
                    ),
                  ),
              ],
            ),
            const SizedBox(height: 12),
            Expanded(
              child: allEmpty
                  ? Center(
                      child: Text('该时段无数据',
                          style: theme.textTheme.bodySmall
                              ?.copyWith(color: scheme.outline)),
                    )
                  : LineChart(_buildData(context)),
            ),
          ],
        ),
      ),
    );
  }

  LineChartData _buildData(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    double minY = double.infinity, maxY = 0;
    final bars = <LineChartBarData>[];
    for (var i = 0; i < series.length; i++) {
      final s = series[i];
      final spots = <FlSpot>[];
      for (var j = 0; j < s.points.length; j++) {
        final v = s.points[j].avg;
        minY = v < minY ? v : minY;
        maxY = v > maxY ? v : maxY;
        spots.add(FlSpot(j.toDouble(), v));
      }
      bars.add(LineChartBarData(
        spots: spots,
        isCurved: false,
        color: _colors[i % _colors.length],
        barWidth: 2,
        dotData: const FlDotData(show: false),
      ));
    }
    if (!minY.isFinite) minY = 0;
    if (maxY <= minY) maxY = minY + 1;
    final pad = (maxY - minY) * 0.1;
    return LineChartData(
      minY: (minY - pad).clamp(0, double.infinity),
      maxY: maxY + pad,
      gridData: FlGridData(
        show: true,
        drawVerticalLine: false,
        getDrawingHorizontalLine: (_) =>
            FlLine(color: scheme.outlineVariant.withValues(alpha: 0.4), strokeWidth: 1),
      ),
      borderData: FlBorderData(show: false),
      titlesData: FlTitlesData(
        topTitles: const AxisTitles(),
        rightTitles: const AxisTitles(),
        bottomTitles: AxisTitles(
          sideTitles: SideTitles(
            showTitles: true,
            reservedSize: 28,
            interval: _xInterval(),
            getTitlesWidget: (value, meta) {
              final idx = value.toInt();
              final points = series.first.points;
              if (idx < 0 || idx >= points.length || idx % _xLabelEvery() != 0) {
                return const SizedBox.shrink();
              }
              return Padding(
                padding: const EdgeInsets.only(top: 6),
                child: Text(
                  formatTimeShort(points[idx].ts),
                  style: TextStyle(fontSize: 10, color: scheme.outline),
                ),
              );
            },
          ),
        ),
        leftTitles: AxisTitles(
          sideTitles: SideTitles(
            showTitles: true,
            reservedSize: 56,
            getTitlesWidget: (value, meta) => Text(
              yFormatter?.call(value) ?? _defaultYFormat(value),
              style: TextStyle(fontSize: 10, color: scheme.outline),
            ),
          ),
        ),
      ),
      lineBarsData: bars,
      lineTouchData: LineTouchData(
        touchTooltipData: LineTouchTooltipData(
          getTooltipItems: (spots) => spots
              .map((s) => LineTooltipItem(
                    '${series[s.barIndex].name} '
                    '${yFormatter?.call(s.y) ?? _defaultYFormat(s.y)}\n'
                    '${formatTime(series[s.barIndex].points[s.x.toInt()].ts)}',
                    const TextStyle(fontSize: 11, color: Colors.white),
                  ))
              .toList(),
        ),
      ),
    );
  }

  int _xLabelEvery() {
    final n = series.first.points.length;
    if (n <= 6) return 1;
    return (n / 5).ceil();
  }

  double _xInterval() {
    final n = series.first.points.length;
    if (n <= 1) return 1;
    return _xLabelEvery().toDouble();
  }

  String _defaultYFormat(double v) {
    if (unit == '%') return '${v.toStringAsFixed(0)}%';
    if (v >= 1000000) return '${(v / 1000000).toStringAsFixed(1)}M';
    if (v >= 1000) return '${(v / 1000).toStringAsFixed(1)}K';
    return v.toStringAsFixed(0);
  }
}
