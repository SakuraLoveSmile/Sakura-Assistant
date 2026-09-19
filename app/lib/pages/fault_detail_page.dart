import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../api/api_client.dart';
import '../api/api_exception.dart';
import '../models/fault.dart';
import '../models/message.dart';
import '../state/data_providers.dart';
import '../state/providers.dart';
import '../widgets/common.dart';
import '../widgets/format.dart';

/// 故障详情：当前轮次事件时间线 + 历史轮次 + 已读/静音操作。
/// （契约无 `GET /faults/:id`：数据来自本地缓存 + 消息时间线推导。）
class FaultDetailPage extends ConsumerWidget {
  const FaultDetailPage({super.key, required this.faultId});

  final String faultId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final detail = ref.watch(faultDetailProvider(faultId));
    return Scaffold(
      appBar: AppBar(title: const Text('故障详情')),
      body: AsyncValueView(
        value: detail,
        onRetry: () => ref.invalidate(faultDetailProvider(faultId)),
        builder: (data) {
          if (data.fault == null && data.events.isEmpty) {
            return const EmptyView(
                icon: Icons.search_off_outlined, text: '本地没有该故障的数据（可能尚未同步）');
          }
          return _Body(data: data, faultId: faultId);
        },
      ),
    );
  }
}

class _Body extends ConsumerWidget {
  const _Body({required this.data, required this.faultId});

  final FaultDetailData data;
  final String faultId;

  Future<void> _act(
    BuildContext context,
    WidgetRef ref,
    Future<Fault> Function(AssistantApi api) action,
  ) async {
    try {
      final updated = await action(ref.read(apiProvider));
      await ref.read(cacheProvider).upsertFault(updated);
      ref.read(cacheRevisionProvider.notifier).bump();
      ref.invalidate(faultDetailProvider(faultId));
    } on ApiException catch (e) {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text('操作失败：${e.message}')));
      }
    }
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final fault = data.fault;
    final theme = Theme.of(context);
    final scheme = theme.colorScheme;
    final groups = data.byIncident;
    final incidents = groups.keys.toList()
      ..sort((a, b) => b.compareTo(a)); // 新轮次在前
    final currentIncident = fault?.incident ?? (incidents.isEmpty ? 1 : incidents.first);
    return ListView(
      padding: const EdgeInsets.only(bottom: 40),
      children: [
        if (fault != null)
          Card(
            margin: const EdgeInsets.all(16),
            child: Padding(
              padding: const EdgeInsets.all(16),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Row(
                    children: [
                      SeverityBadge(severity: fault.severity),
                      const SizedBox(width: 8),
                      Expanded(
                        child: Text(fault.title,
                            style: theme.textTheme.titleLarge),
                      ),
                      if (fault.muted)
                        Icon(Icons.notifications_off_outlined,
                            color: scheme.outline),
                    ],
                  ),
                  const SizedBox(height: 8),
                  Wrap(
                    spacing: 12,
                    runSpacing: 4,
                    children: [
                      _Meta(text: '来源 ${fault.sourceName}'),
                      _Meta(text: 'faultKey ${fault.faultKey}'),
                      _Meta(text: '第 ${fault.incident} 轮'),
                      _Meta(text: fault.isOpen ? '进行中' : '已恢复'),
                    ],
                  ),
                  const SizedBox(height: 8),
                  Text(
                    '开启 ${formatTime(fault.openedAt)} · 最近事件 ${formatTime(fault.lastEventAt)}'
                    '${fault.resolvedAt != null ? ' · 恢复 ${formatTime(fault.resolvedAt)}' : ''}',
                    style: theme.textTheme.bodySmall
                        ?.copyWith(color: scheme.outline),
                  ),
                  if (fault.summary != null && fault.summary!.isNotEmpty) ...[
                    const SizedBox(height: 8),
                    Text(fault.summary!,
                        style: theme.textTheme.bodySmall
                            ?.copyWith(color: scheme.outline)),
                  ],
                  const Divider(height: 24),
                  Row(
                    children: [
                      if (fault.isOpen && !fault.isRead)
                        TextButton.icon(
                          onPressed: () => _act(context, ref,
                              (api) => api.markFaultRead(fault.id)),
                          icon: const Icon(Icons.done),
                          label: const Text('标记已读'),
                        ),
                      TextButton.icon(
                        onPressed: () => _act(
                            context,
                            ref,
                            (api) => fault.muted
                                ? api.unmuteFault(fault.id)
                                : api.muteFault(fault.id)),
                        icon: Icon(fault.muted
                            ? Icons.notifications_active_outlined
                            : Icons.notifications_off_outlined),
                        label: Text(fault.muted ? '取消静音' : '静音'),
                      ),
                    ],
                  ),
                ],
              ),
            ),
          )
        else
          const Padding(
            padding: EdgeInsets.all(16),
            child: Text('故障对象未在本地缓存（可能已被清理），以下为相关事件'),
          ),
        for (final incident in incidents) ...[
          SectionTitle(
            text: incident == currentIncident && (fault?.isOpen ?? true)
                ? '第 $incident 轮（进行中）'
                : '第 $incident 轮',
            trailing: Text('${groups[incident]!.length} 条',
                style: theme.textTheme.bodySmall),
          ),
          Card(
            margin: const EdgeInsets.symmetric(horizontal: 16),
            child: Column(
              children: [
                for (var i = 0; i < groups[incident]!.length; i++)
                  _EventTile(
                    message: groups[incident]![i],
                    isLast: i == groups[incident]!.length - 1,
                  ),
              ],
            ),
          ),
        ],
      ],
    );
  }
}

class _Meta extends StatelessWidget {
  const _Meta({required this.text});

  final String text;

  @override
  Widget build(BuildContext context) {
    return Text(
      text,
      style: Theme.of(context)
          .textTheme
          .labelMedium
          ?.copyWith(color: Theme.of(context).colorScheme.outline),
    );
  }
}

/// 时间线条目：左侧时间轴 + severity 点，右侧标题/时间/正文摘要。
class _EventTile extends StatelessWidget {
  const _EventTile({required this.message, required this.isLast});

  final Message message;
  final bool isLast;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final scheme = theme.colorScheme;
    return IntrinsicHeight(
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          SizedBox(
            width: 40,
            child: Column(
              children: [
                const SizedBox(height: 18),
                SeverityDot(severity: message.severity),
                if (!isLast)
                  Expanded(
                    child: Container(
                      width: 1,
                      color: scheme.outlineVariant,
                      margin: const EdgeInsets.symmetric(vertical: 4),
                    ),
                  ),
              ],
            ),
          ),
          Expanded(
            child: Padding(
              padding: const EdgeInsets.fromLTRB(0, 14, 16, 14),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(message.title,
                      style: theme.textTheme.bodyMedium
                          ?.copyWith(fontWeight: FontWeight.w600)),
                  const SizedBox(height: 2),
                  Text(
                    '${kindLabel(message.kind)} · ${formatTime(message.occurredAt)}',
                    style: theme.textTheme.bodySmall
                        ?.copyWith(color: scheme.outline),
                  ),
                  if (message.body != null && message.body!.isNotEmpty) ...[
                    const SizedBox(height: 4),
                    Text(
                      message.body!,
                      maxLines: 3,
                      overflow: TextOverflow.ellipsis,
                      style: theme.textTheme.bodySmall,
                    ),
                  ],
                ],
              ),
            ),
          ),
        ],
      ),
    );
  }
}
