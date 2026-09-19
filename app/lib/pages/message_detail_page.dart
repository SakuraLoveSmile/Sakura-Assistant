import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../api/api_client.dart';
import '../models/fault.dart';
import '../models/message.dart';
import '../state/data_providers.dart';
import '../state/providers.dart';
import '../widgets/common.dart';
import '../widgets/format.dart';

/// 消息详情：正文 / 错误摘要 / 截图（懒加载+失败占位）/ 日志预览 /
/// 已读切换 / 所属故障卡（静音切换）。
class MessageDetailPage extends ConsumerWidget {
  const MessageDetailPage({super.key, required this.messageId});

  final String messageId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final detail = ref.watch(messageDetailProvider(messageId));
    return Scaffold(
      appBar: AppBar(title: const Text('消息详情')),
      body: AsyncValueView(
        value: detail,
        onRetry: () => ref.invalidate(messageDetailProvider(messageId)),
        builder: (data) => _Body(detail: data, messageId: messageId),
      ),
    );
  }
}

class _Body extends ConsumerWidget {
  const _Body({required this.detail, required this.messageId});

  final MessageDetail detail;
  final String messageId;

  Future<void> _toggleRead(BuildContext context, WidgetRef ref) async {
    final api = ref.read(apiProvider);
    try {
      final updated = detail.message.isRead
          ? await api.markMessageUnread(messageId)
          : await api.markMessageRead(messageId);
      // 写入本地缓存保持一致（等 sync 收敛）。
      await ref.read(cacheProvider).upsertMessage(updated);
      ref.read(cacheRevisionProvider.notifier).bump();
      ref.invalidate(messageDetailProvider(messageId));
    } on Exception catch (e) {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text('操作失败：$e')));
      }
    }
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final m = detail.message;
    final theme = Theme.of(context);
    final scheme = theme.colorScheme;
    final screenshots =
        m.attachments.where((a) => a.isImage || a.kind == 'screenshot').toList();
    final logs = m.attachments
        .where((a) => !screenshots.contains(a))
        .toList();
    return ListView(
      padding: const EdgeInsets.only(bottom: 40),
      children: [
        Padding(
          padding: const EdgeInsets.all(16),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Row(
                children: [
                  SeverityBadge(severity: m.severity),
                  const SizedBox(width: 8),
                  Text(kindLabel(m.kind),
                      style: theme.textTheme.labelMedium
                          ?.copyWith(color: scheme.outline)),
                  const Spacer(),
                  Text(m.sourceName, style: theme.textTheme.labelMedium),
                ],
              ),
              const SizedBox(height: 12),
              Text(m.title, style: theme.textTheme.titleLarge),
              const SizedBox(height: 8),
              Text(
                '发生 ${formatTime(m.occurredAt)} · 接收 ${formatTime(m.receivedAt)}',
                style:
                    theme.textTheme.bodySmall?.copyWith(color: scheme.outline),
              ),
              if (m.feedbackId != null) ...[
                const SizedBox(height: 4),
                Text('反馈 ID：${m.feedbackId}',
                    style: theme.textTheme.bodySmall
                        ?.copyWith(color: scheme.outline)),
              ],
            ],
          ),
        ),
        if (m.body != null && m.body!.isNotEmpty)
          Card(
            margin: const EdgeInsets.symmetric(horizontal: 16),
            child: Padding(
              padding: const EdgeInsets.all(16),
              child: SelectableText(m.body!,
                  style: theme.textTheme.bodyMedium
                      ?.copyWith(height: 1.5)),
            ),
          ),
        if (m.ref != null && m.ref!.isNotEmpty && m.feedbackId == null)
          Card(
            margin: const EdgeInsets.fromLTRB(16, 12, 16, 0),
            child: Padding(
              padding: const EdgeInsets.all(16),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text('引用信息', style: theme.textTheme.titleSmall),
                  const SizedBox(height: 6),
                  for (final e in m.ref!.entries)
                    Text('${e.key}: ${e.value}',
                        style: theme.textTheme.bodySmall),
                ],
              ),
            ),
          ),
        if (screenshots.isNotEmpty) ...[
          const SectionTitle(text: '截图'),
          for (final att in screenshots)
            _ScreenshotView(messageId: m.id, attachment: att),
        ],
        if (logs.isNotEmpty) ...[
          const SectionTitle(text: '附件 / 日志'),
          Card(
            margin: const EdgeInsets.symmetric(horizontal: 16),
            child: Column(
              children: [
                for (final att in logs)
                  _AttachmentTile(messageId: m.id, attachment: att),
              ],
            ),
          ),
        ],
        if (detail.fault != null) ...[
          const SectionTitle(text: '所属故障'),
          _FaultCard(fault: detail.fault!),
        ],
        const SizedBox(height: 24),
        Padding(
          padding: const EdgeInsets.symmetric(horizontal: 16),
          child: OutlinedButton.icon(
            onPressed: () => _toggleRead(context, ref),
            icon: Icon(m.isRead
                ? Icons.mark_email_unread_outlined
                : Icons.mark_email_read_outlined),
            label: Text(m.isRead ? '标记为未读' : '标记为已读'),
          ),
        ),
      ],
    );
  }
}

/// 截图：按需懒加载（带 Bearer），失败显示「附件不可用」占位，绝不崩溃。
class _ScreenshotView extends ConsumerWidget {
  const _ScreenshotView({required this.messageId, required this.attachment});

  final String messageId;
  final Attachment attachment;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final api = ref.read(apiProvider);
    final uri = api.attachmentUri(messageId, attachment.id);
    final token = api.session?.token;
    return Padding(
      padding: const EdgeInsets.symmetric(horizontal: 16),
      child: Card(
        clipBehavior: Clip.antiAlias,
        child: InkWell(
          onTap: () => _showFullImage(context, uri.toString(), token),
          child: ConstrainedBox(
            constraints: const BoxConstraints(maxHeight: 280, minHeight: 80),
            child: Image.network(
              uri.toString(),
              headers:
                  token == null ? null : {'Authorization': 'Bearer $token'},
              fit: BoxFit.contain,
              loadingBuilder: (context, child, progress) {
                if (progress == null) return child;
                return const SizedBox(
                  height: 120,
                  child: Center(child: CircularProgressIndicator()),
                );
              },
              errorBuilder: (context, error, stack) =>
                  const _AttachmentUnavailable(),
            ),
          ),
        ),
      ),
    );
  }

  void _showFullImage(BuildContext context, String url, String? token) {
    Navigator.of(context).push(MaterialPageRoute<void>(
      builder: (_) => Scaffold(
        appBar: AppBar(title: Text(attachment.filename)),
        body: InteractiveViewer(
          maxScale: 5,
          child: Center(
            child: Image.network(
              url,
              headers:
                  token == null ? null : {'Authorization': 'Bearer $token'},
              errorBuilder: (context, error, stack) =>
                  const _AttachmentUnavailable(),
            ),
          ),
        ),
      ),
    ));
  }
}

class _AttachmentUnavailable extends StatelessWidget {
  const _AttachmentUnavailable();

  @override
  Widget build(BuildContext context) {
    return SizedBox(
      height: 120,
      child: Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Icon(Icons.broken_image_outlined,
                size: 32, color: Theme.of(context).colorScheme.outline),
            const SizedBox(height: 6),
            Text('附件不可用',
                style: Theme.of(context)
                    .textTheme
                    .bodySmall
                    ?.copyWith(color: Theme.of(context).colorScheme.outline)),
          ],
        ),
      ),
    );
  }
}

/// 日志 / 其他附件条目：点开预览文本；失败显示「附件不可用」；可复制。
class _AttachmentTile extends ConsumerWidget {
  const _AttachmentTile({required this.messageId, required this.attachment});

  final String messageId;
  final Attachment attachment;

  Future<void> _open(BuildContext context, WidgetRef ref) async {
    showDialog<void>(
      context: context,
      builder: (_) => _AttachmentPreview(
        messageId: messageId,
        attachment: attachment,
      ),
    );
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    return ListTile(
      leading: Icon(
        attachment.isLog ? Icons.article_outlined : Icons.attach_file,
        color: Theme.of(context).colorScheme.primary,
      ),
      title: Text(attachment.filename,
          maxLines: 1, overflow: TextOverflow.ellipsis),
      subtitle: Text('${attachment.mime} · ${formatBytes(attachment.byteSize)}'),
      trailing: const Icon(Icons.chevron_right),
      onTap: () => _open(context, ref),
    );
  }
}

class _AttachmentPreview extends ConsumerStatefulWidget {
  const _AttachmentPreview({required this.messageId, required this.attachment});

  final String messageId;
  final Attachment attachment;

  @override
  ConsumerState<_AttachmentPreview> createState() =>
      _AttachmentPreviewState();
}

class _AttachmentPreviewState extends ConsumerState<_AttachmentPreview> {
  late Future<String> _future;

  @override
  void initState() {
    super.initState();
    _future = _load();
  }

  Future<String> _load() async {
    final api = ref.read(apiProvider);
    final res =
        await api.fetchAttachment(widget.messageId, widget.attachment.id);
    if (res.bytes.length > 256 * 1024) {
      return '（文件过大，仅显示前 256KiB）\n\n${utf8.decode(res.bytes.sublist(0, 256 * 1024), allowMalformed: true)}';
    }
    return utf8.decode(res.bytes, allowMalformed: true);
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Text(widget.attachment.filename),
      content: SizedBox(
        width: double.maxFinite,
        child: FutureBuilder<String>(
          future: _future,
          builder: (context, snap) {
            if (snap.hasError) {
              return const Row(
                children: [
                  Icon(Icons.error_outline, size: 18),
                  SizedBox(width: 8),
                  Text('附件不可用'),
                ],
              );
            }
            if (!snap.hasData) {
              return const Center(child: CircularProgressIndicator());
            }
            return SingleChildScrollView(
              child: SelectableText(
                snap.data!,
                style: const TextStyle(fontFamily: 'monospace', fontSize: 12),
              ),
            );
          },
        ),
      ),
      actions: [
        TextButton(
          onPressed: () async {
            final text = await _future.catchError((_) => '');
            if (text.isNotEmpty) {
              await Clipboard.setData(ClipboardData(text: text));
              if (context.mounted) {
                ScaffoldMessenger.of(context)
                    .showSnackBar(const SnackBar(content: Text('已复制到剪贴板')));
              }
            }
          },
          child: const Text('复制'),
        ),
        FilledButton(
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('关闭'),
        ),
      ],
    );
  }
}

/// 所属故障卡：标题、轮次、状态、静音切换、跳故障详情。
class _FaultCard extends ConsumerWidget {
  const _FaultCard({required this.fault});

  final Fault fault;

  Future<void> _toggleMute(BuildContext context, WidgetRef ref) async {
    final api = ref.read(apiProvider);
    try {
      final updated =
          fault.muted ? await api.unmuteFault(fault.id) : await api.muteFault(fault.id);
      await ref.read(cacheProvider).upsertFault(updated);
      ref.read(cacheRevisionProvider.notifier).bump();
    } on Exception catch (e) {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text('操作失败：$e')));
      }
    }
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final theme = Theme.of(context);
    return Card(
      margin: const EdgeInsets.symmetric(horizontal: 16),
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
                  child: Text(fault.title, style: theme.textTheme.titleMedium),
                ),
                if (fault.muted)
                  Icon(Icons.notifications_off_outlined,
                      size: 18, color: theme.colorScheme.outline),
              ],
            ),
            const SizedBox(height: 8),
            Text(
              '第 ${fault.incident} 轮 · ${fault.isOpen ? '进行中' : '已恢复'} · '
              '${fault.eventCount} 条事件 · 最近 ${formatRelative(fault.lastEventAt)}',
              style: theme.textTheme.bodySmall
                  ?.copyWith(color: theme.colorScheme.outline),
            ),
            const SizedBox(height: 12),
            Row(
              children: [
                TextButton.icon(
                  onPressed: () => _toggleMute(context, ref),
                  icon: Icon(fault.muted
                      ? Icons.notifications_active_outlined
                      : Icons.notifications_off_outlined),
                  label: Text(fault.muted ? '取消静音' : '静音'),
                ),
                const Spacer(),
                TextButton(
                  onPressed: () =>
                      context.push('/faults/${Uri.encodeComponent(fault.id)}'),
                  child: const Text('查看故障详情'),
                ),
              ],
            ),
          ],
        ),
      ),
    );
  }
}
