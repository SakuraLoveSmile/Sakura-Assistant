import 'package:flutter/material.dart';

import '../models/source.dart';
import 'format.dart';

/// 首页设备状态卡：CPU/内存/磁盘/uptime + 能力标签，
/// offline 与「不支持/采集失败」正确呈现（缺失指标绝不显示正常）。
class DeviceCard extends StatelessWidget {
  const DeviceCard({super.key, required this.source, this.onTap});

  final Source source;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    final summary = source.summary;
    final online = source.isOnline;
    return Card(
      child: InkWell(
        onTap: source.isDevice ? onTap : null,
        child: Padding(
          padding: const EdgeInsets.all(16),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Row(
                children: [
                  Icon(
                    source.isDevice
                        ? Icons.dns_outlined
                        : Icons.forum_outlined,
                    size: 20,
                    color: scheme.primary,
                  ),
                  const SizedBox(width: 8),
                  Expanded(
                    child: Text(source.name,
                        style: Theme.of(context).textTheme.titleMedium,
                        overflow: TextOverflow.ellipsis),
                  ),
                  _StatusChip(online: online),
                ],
              ),
              const SizedBox(height: 4),
              Text(
                [
                  if (source.hostname != null) source.hostname!,
                  if (source.agentVersion != null) 'v${source.agentVersion}',
                  if (source.lastSeenAt != null)
                    '最后上报 ${formatRelative(source.lastSeenAt)}'
                  else
                    '从未上报',
                ].join(' · '),
                style: Theme.of(context)
                    .textTheme
                    .bodySmall
                    ?.copyWith(color: scheme.outline),
              ),
              if (source.isDevice) ...[
                const Divider(height: 20),
                if (!online)
                  _OfflineNote(lastSeen: source.lastSeenAt)
                else if (summary == null)
                  Text('暂无采样数据',
                      style: Theme.of(context)
                          .textTheme
                          .bodySmall
                          ?.copyWith(color: scheme.outline))
                else
                  _MetricsRow(summary: summary),
                const SizedBox(height: 10),
                _CapabilityChips(source: source),
              ] else ...[
                const Divider(height: 20),
                Text(
                  source.isOnline ? '事件来源在线' : '事件来源离线',
                  style: Theme.of(context)
                      .textTheme
                      .bodySmall
                      ?.copyWith(color: scheme.outline),
                ),
              ],
            ],
          ),
        ),
      ),
    );
  }
}

class _StatusChip extends StatelessWidget {
  const _StatusChip({required this.online});

  final bool online;

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    final color = online ? const Color(0xFF2E9E5B) : scheme.outline;
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.12),
        borderRadius: BorderRadius.circular(6),
      ),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: [
          Container(
            width: 6,
            height: 6,
            decoration: BoxDecoration(color: color, shape: BoxShape.circle),
          ),
          const SizedBox(width: 4),
          Text(online ? '在线' : '离线',
              style:
                  TextStyle(color: color, fontSize: 12, fontWeight: FontWeight.w600)),
        ],
      ),
    );
  }
}

class _OfflineNote extends StatelessWidget {
  const _OfflineNote({this.lastSeen});

  final DateTime? lastSeen;

  @override
  Widget build(BuildContext context) {
    return Row(
      children: [
        Icon(Icons.cloud_off_outlined,
            size: 16, color: Theme.of(context).colorScheme.outline),
        const SizedBox(width: 6),
        Expanded(
          child: Text(
            lastSeen == null
                ? '设备离线，尚无上报数据'
                : '设备离线，数据停留在 ${formatTimeShort(lastSeen)}',
            style: Theme.of(context)
                .textTheme
                .bodySmall
                ?.copyWith(color: Theme.of(context).colorScheme.outline),
          ),
        ),
      ],
    );
  }
}

class _MetricsRow extends StatelessWidget {
  const _MetricsRow({required this.summary});

  final SourceSummary summary;

  @override
  Widget build(BuildContext context) {
    return Row(
      children: [
        _Metric(label: 'CPU', value: formatPercent(summary.cpuPercent)),
        _Metric(label: '内存', value: formatPercent(summary.memPercent)),
        _Metric(
          label: '磁盘',
          value: summary.disks.isEmpty
              ? '—'
              : summary.disks
                  .map((d) => '${d.mount} ${formatPercent(d.percent)}')
                  .join('\n'),
        ),
        _Metric(label: '运行', value: formatUptime(summary.uptimeSeconds)),
      ],
    );
  }
}

class _Metric extends StatelessWidget {
  const _Metric({required this.label, required this.value});

  final String label;
  final String value;

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    return Expanded(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(label,
              style: Theme.of(context)
                  .textTheme
                  .labelSmall
                  ?.copyWith(color: scheme.outline)),
          const SizedBox(height: 2),
          Text(
            value,
            style: Theme.of(context)
                .textTheme
                .titleSmall
                ?.copyWith(fontWeight: FontWeight.w600),
          ),
        ],
      ),
    );
  }
}

/// 能力标签行：ok=绿、asleep=蓝、unsupported=灰、failed=红，缺席不显示。
class _CapabilityChips extends StatelessWidget {
  const _CapabilityChips({required this.source});

  final Source source;

  static const _labels = {
    'containers': '容器',
    'smart': 'SMART',
    'storagePool': '存储池',
    'network': '网络',
  };

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    final chips = <Widget>[];
    for (final entry in _labels.entries) {
      final cap = source.capabilities[entry.key];
      if (cap == null) continue;
      final (color, text) = switch (cap) {
        'ok' => (const Color(0xFF2E9E5B), '${entry.value}正常'),
        'asleep' => (const Color(0xFF4E7DBF), '${entry.value}休眠'),
        'unsupported' => (scheme.outline, '${entry.value}不支持'),
        'failed' => (scheme.error, '${entry.value}采集失败'),
        _ => (scheme.outline, '${entry.value}$cap'),
      };
      chips.add(_Chip(color: color, text: text));
    }
    if (chips.isEmpty) return const SizedBox.shrink();
    return Wrap(spacing: 6, runSpacing: 4, children: chips);
  }
}

class _Chip extends StatelessWidget {
  const _Chip({required this.color, required this.text});

  final Color color;
  final String text;

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.12),
        borderRadius: BorderRadius.circular(6),
      ),
      child: Text(text,
          style: TextStyle(color: color, fontSize: 11, fontWeight: FontWeight.w500)),
    );
  }
}
