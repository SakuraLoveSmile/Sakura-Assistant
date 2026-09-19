import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../api/api_exception.dart';
import '../models/feedback.dart';
import '../state/feedback_providers.dart';
import '../state/providers.dart';
import '../widgets/common.dart';
import '../widgets/format.dart';

/// 反馈详情（api-v1 §3.1）：全文 / 状态与版本 / 截图内联 / 日志预览 /
/// `allowedActions` 渲染管理操作。
///
/// 操作语义（feedback-integration §4.2 + §6）：
/// - 按钮按 `allowedActions` 渲染；`trash` 二次确认；执行中禁用防重复；
/// - 成功 / 幂等回放 → 响应 `detail` 就地刷新并触发列表失效；
/// - 冲突（version/revision/invalid_state）→ 响应 `detail` 刷新 + 提示；
/// - 超时 / 网络错 → 「结果待确认」+ 自动拉详情核对，绝不自动重发；
/// - `capabilities.manage=false` / 不支持 / 未配置 → 管理区只读提示；
/// - 断网（connectivity）时操作禁用。
class FeedbackDetailPage extends ConsumerStatefulWidget {
  const FeedbackDetailPage({
    super.key,
    required this.sourceId,
    required this.feedbackId,
  });

  final String sourceId;
  final String feedbackId;

  @override
  ConsumerState<FeedbackDetailPage> createState() =>
      _FeedbackDetailPageState();
}

class _FeedbackDetailPageState extends ConsumerState<FeedbackDetailPage> {
  /// 正在执行的动作（wire 值）；非空时全部操作按钮禁用（防重复点击）。
  String? _busy;

  FeedbackRef get _target =>
      (sourceId: widget.sourceId, feedbackId: widget.feedbackId);

  Future<void> _runAction(FeedbackAction action) async {
    if (_busy != null) return;
    if (action == FeedbackAction.trash) {
      final ok = await showDialog<bool>(
        context: context,
        builder: (ctx) => AlertDialog(
          title: const Text('确认移入回收站'),
          content: const Text('移入回收站后可在「回收站」视图恢复。确定继续？'),
          actions: [
            TextButton(
                onPressed: () => Navigator.of(ctx).pop(false),
                child: const Text('取消')),
            FilledButton(
                onPressed: () => Navigator.of(ctx).pop(true),
                child: const Text('移入回收站')),
          ],
        ),
      );
      if (ok != true || !mounted) return;
    }
    setState(() => _busy = action.wire);
    final outcome = await ref
        .read(feedbackDetailProvider(_target).notifier)
        .runAction(action);
    if (!mounted) return;
    setState(() => _busy = null);
    ScaffoldMessenger.of(context).showSnackBar(SnackBar(
      content: Text(outcome.message),
      action: outcome.status == FeedbackActionStatus.configNeeded
          ? SnackBarAction(
              label: '接入管理',
              onPressed: () => context.push('/sources'),
            )
          : null,
    ));
  }

  @override
  Widget build(BuildContext context) {
    final detail = ref.watch(feedbackDetailProvider(_target));
    final online = ref.watch(onlineProvider).value ?? true;
    return Scaffold(
      appBar: AppBar(
        title: const Text('反馈详情'),
        actions: [
          IconButton(
            tooltip: '刷新',
            icon: const Icon(Icons.refresh),
            onPressed: () =>
                ref.read(feedbackDetailProvider(_target).notifier).refresh(),
          ),
        ],
      ),
      body: AsyncValueView(
        value: detail,
        onRetry: () =>
            ref.read(feedbackDetailProvider(_target).notifier).refresh(),
        errorBuilder: (error) => _errorView(error),
        builder: (d) => _DetailBody(
          detail: d,
          sourceId: widget.sourceId,
          online: online,
          busy: _busy,
          onAction: _runAction,
          onCopy: _copyText,
        ),
      ),
    );
  }

  Future<void> _copyText(String label, String text) async {
    await Clipboard.setData(ClipboardData(text: text));
    if (mounted) {
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text('已复制$label')));
    }
  }

  Widget _errorView(Object error) {
    final theme = Theme.of(context);
    IconData icon = Icons.cloud_off_outlined;
    String title = '加载失败';
    String detail = '$error';
    if (error is ApiException) {
      detail = error.message;
      switch (error.feedbackCategory) {
        case FeedbackErrorCategory.needsConfig:
          icon = Icons.vpn_key_off_outlined;
          title = error.isFeedbackMgmtNotConfigured ? '未配置管理凭证' : '未配置回连地址';
        case FeedbackErrorCategory.needsUpgrade:
          icon = Icons.system_update_alt_outlined;
          title = '需升级 Feedback 服务';
        case FeedbackErrorCategory.uncertain:
          icon = Icons.help_outline;
          title = '结果待确认';
        case FeedbackErrorCategory.retryable:
          title = error.isFeedbackUnavailable ? '反馈服务不可达' : '加载失败';
        case FeedbackErrorCategory.conflict:
        case FeedbackErrorCategory.other:
          if (error.isNotFound) {
            icon = Icons.search_off_outlined;
            title = '反馈不存在或已被删除';
          }
      }
    }
    return Center(
      child: Padding(
        padding: const EdgeInsets.all(24),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Icon(icon, size: 40, color: theme.colorScheme.outline),
            const SizedBox(height: 12),
            Text(title, style: theme.textTheme.titleMedium),
            const SizedBox(height: 6),
            Text(detail,
                textAlign: TextAlign.center,
                style: theme.textTheme.bodySmall
                    ?.copyWith(color: theme.colorScheme.outline)),
            const SizedBox(height: 16),
            FilledButton.tonalIcon(
              onPressed: () => ref
                  .read(feedbackDetailProvider(_target).notifier)
                  .refresh(),
              icon: const Icon(Icons.refresh),
              label: const Text('重试'),
            ),
          ],
        ),
      ),
    );
  }
}

class _DetailBody extends StatelessWidget {
  const _DetailBody({
    required this.detail,
    required this.sourceId,
    required this.online,
    required this.busy,
    required this.onAction,
    required this.onCopy,
  });

  final FeedbackDetail detail;

  /// 路由带来的来源 ID（响应缺 `sourceId` 字段时仍可用）。
  final String sourceId;
  final bool online;
  final String? busy;
  final void Function(FeedbackAction action) onAction;
  final void Function(String label, String text) onCopy;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final scheme = theme.colorScheme;
    final d = detail;
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
                  _Badge(
                    label: feedbackStatusLabel(d.status),
                    color: feedbackStatusColor(d.status, scheme),
                  ),
                  if (d.mgmtState != null) ...[
                    const SizedBox(width: 6),
                    _Badge(
                      label: feedbackMgmtLabel(d.mgmtState),
                      color: scheme.primary,
                    ),
                  ],
                  if (d.resumePaused) ...[
                    const SizedBox(width: 6),
                    _Badge(label: '已暂停', color: scheme.error),
                  ],
                  const Spacer(),
                  if (d.appName != null || d.appId.isNotEmpty)
                    Text(d.appName ?? d.appId,
                        style: theme.textTheme.labelMedium),
                ],
              ),
              const SizedBox(height: 12),
              Text(d.displayTitle, style: theme.textTheme.titleLarge),
              const SizedBox(height: 8),
              Row(
                children: [
                  Expanded(
                    child: Text('ID：${d.id}',
                        style: theme.textTheme.bodySmall
                            ?.copyWith(color: scheme.outline),
                        overflow: TextOverflow.ellipsis),
                  ),
                  IconButton(
                    icon: const Icon(Icons.copy_outlined, size: 16),
                    tooltip: '复制反馈 ID',
                    visualDensity: VisualDensity.compact,
                    onPressed: () => onCopy('反馈 ID', d.id),
                  ),
                ],
              ),
              Text(
                '创建 ${formatTime(d.createdAt)} · 更新 ${formatTime(d.updatedAt)}',
                style:
                    theme.textTheme.bodySmall?.copyWith(color: scheme.outline),
              ),
            ],
          ),
        ),
        if (d.errorSummary != null && d.errorSummary!.isNotEmpty)
          Card(
            margin: const EdgeInsets.symmetric(horizontal: 16),
            child: Padding(
              padding: const EdgeInsets.all(16),
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Icon(Icons.error_outline, size: 18, color: scheme.error),
                  const SizedBox(width: 8),
                  Expanded(
                    child: SelectableText(
                      d.errorSummary!,
                      style: theme.textTheme.bodySmall
                          ?.copyWith(color: scheme.error),
                    ),
                  ),
                ],
              ),
            ),
          ),
        if (d.text != null && d.text!.isNotEmpty) ...[
          const SectionTitle(text: '正文'),
          Card(
            margin: const EdgeInsets.symmetric(horizontal: 16),
            child: Padding(
              padding: const EdgeInsets.all(16),
              child: SelectableText(d.text!,
                  style:
                      theme.textTheme.bodyMedium?.copyWith(height: 1.5)),
            ),
          ),
        ],
        const SectionTitle(text: '状态'),
        Card(
          margin: const EdgeInsets.symmetric(horizontal: 16),
          child: Padding(
            padding: const EdgeInsets.all(16),
            child: Column(
              children: [
                _MetaRow(label: '处理状态', value: feedbackStatusLabel(d.status)),
                _MetaRow(
                    label: '问题单', value: feedbackIssueLabel(d.issueStatus)),
                _MetaRow(
                    label: '生命周期',
                    value: feedbackMgmtLabel(d.mgmtState)),
                _MetaRow(
                    label: '收集进度',
                    value: feedbackCollectionLabel(d.collectionState)),
                _MetaRow(
                    label: '归档阶段',
                    value: feedbackArchiveStageLabel(d.archiveStage)),
                if (d.lifecycleVersion != null)
                  _MetaRow(label: '生命周期版本', value: 'v${d.lifecycleVersion}'),
                if (d.revision != null)
                  _MetaRow(label: '归档数据版本', value: 'r${d.revision}'),
                if (d.archivedAt != null)
                  _MetaRow(label: '归档于', value: formatTime(d.archivedAt)),
                if (d.trashedAt != null)
                  _MetaRow(label: '移入回收站', value: formatTime(d.trashedAt)),
                if (d.sourceName != null)
                  _MetaRow(label: '来源', value: d.sourceName!),
                if (d.kaneoTaskUrl != null && d.kaneoTaskUrl!.isNotEmpty)
                  _MetaRow(
                    label: 'Kaneo 任务',
                    value: d.kaneoTaskUrl!,
                    onCopy: () => onCopy('Kaneo 链接', d.kaneoTaskUrl!),
                  ),
              ],
            ),
          ),
        ),
        if (d.hasScreenshot) ...[
          const SectionTitle(text: '截图'),
          _FeedbackScreenshot(detail: d, sourceId: sourceId),
        ],
        if (d.logs.isNotEmpty) ...[
          const SectionTitle(text: '日志附件'),
          Card(
            margin: const EdgeInsets.symmetric(horizontal: 16),
            child: Column(
              children: [
                for (final log in d.logs)
                  _FeedbackLogTile(
                      detail: d, log: log, sourceId: sourceId),
              ],
            ),
          ),
        ],
        const SectionTitle(text: '管理操作'),
        _ManageSection(
          detail: d,
          online: online,
          busy: busy,
          onAction: onAction,
        ),
      ],
    );
  }
}

class _MetaRow extends StatelessWidget {
  const _MetaRow({required this.label, required this.value, this.onCopy});

  final String label;
  final String value;
  final VoidCallback? onCopy;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 4),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(label,
              style: theme.textTheme.bodyMedium
                  ?.copyWith(color: theme.colorScheme.outline)),
          const SizedBox(width: 12),
          Expanded(
            child: SelectableText(
              value,
              textAlign: TextAlign.end,
              style: theme.textTheme.bodySmall,
            ),
          ),
          if (onCopy != null)
            GestureDetector(
              onTap: onCopy,
              child: Padding(
                padding: const EdgeInsets.only(left: 6),
                child: Icon(Icons.copy_outlined,
                    size: 14, color: theme.colorScheme.outline),
              ),
            ),
        ],
      ),
    );
  }
}

class _Badge extends StatelessWidget {
  const _Badge({required this.label, required this.color});

  final String label;
  final Color color;

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 2),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.12),
        borderRadius: BorderRadius.circular(6),
      ),
      child: Text(
        label,
        style: TextStyle(
            color: color, fontSize: 12, fontWeight: FontWeight.w600),
      ),
    );
  }
}

/// 管理操作区：按 allowedActions 渲染；不可管理时给只读说明。
class _ManageSection extends StatelessWidget {
  const _ManageSection({
    required this.detail,
    required this.online,
    required this.busy,
    required this.onAction,
  });

  final FeedbackDetail detail;
  final bool online;
  final String? busy;
  final void Function(FeedbackAction action) onAction;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final scheme = theme.colorScheme;
    final actions = detail.knownActions;
    return Card(
      margin: const EdgeInsets.symmetric(horizontal: 16),
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            if (!online)
              Padding(
                padding: const EdgeInsets.only(bottom: 8),
                child: Row(
                  children: [
                    Icon(Icons.wifi_off_outlined,
                        size: 16, color: scheme.outline),
                    const SizedBox(width: 6),
                    Expanded(
                      child: Text('当前无网络连接，操作已禁用',
                          style: theme.textTheme.bodySmall
                              ?.copyWith(color: scheme.outline)),
                    ),
                  ],
                ),
              ),
            if (!detail.manage)
              Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Icon(Icons.lock_outline, size: 18, color: scheme.outline),
                  const SizedBox(width: 8),
                  Expanded(
                    child: Text(
                      detail.hasMgmtFields
                          ? '管理不可用：该来源未配置管理凭证（mgmtKey）或服务端管理面未挂载。'
                              '可在「接入管理」为该来源配置后重试。'
                          : '当前 Feedback 服务版本过旧，不支持管理操作（需 v0.6.0+）；'
                              '反馈内容与附件仍可查看。',
                      style: theme.textTheme.bodySmall
                          ?.copyWith(color: scheme.outline),
                    ),
                  ),
                ],
              )
            else if (actions.isEmpty)
              Text('当前状态下没有可执行的管理操作',
                  style: theme.textTheme.bodySmall
                      ?.copyWith(color: scheme.outline))
            else
              Wrap(
                spacing: 8,
                runSpacing: 8,
                children: [
                  for (final action in actions)
                    _ActionButton(
                      action: action,
                      running: busy == action.wire,
                      enabled: online && busy == null,
                      onPressed: () => onAction(action),
                    ),
                ],
              ),
          ],
        ),
      ),
    );
  }
}

class _ActionButton extends StatelessWidget {
  const _ActionButton({
    required this.action,
    required this.running,
    required this.enabled,
    required this.onPressed,
  });

  final FeedbackAction action;
  final bool running;
  final bool enabled;
  final VoidCallback onPressed;

  IconData get _icon => switch (action) {
        FeedbackAction.archive => Icons.archive_outlined,
        FeedbackAction.unarchive => Icons.unarchive_outlined,
        FeedbackAction.trash => Icons.delete_outline,
        FeedbackAction.restore => Icons.restore_from_trash_outlined,
        FeedbackAction.resumeProcessing => Icons.play_circle_outline,
        FeedbackAction.retry => Icons.refresh,
        FeedbackAction.recheck => Icons.fact_check_outlined,
      };

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    final danger = action == FeedbackAction.trash;
    return FilledButton.tonalIcon(
      onPressed: enabled ? onPressed : null,
      style: danger
          ? FilledButton.styleFrom(
              foregroundColor: scheme.error,
              backgroundColor: scheme.errorContainer,
            )
          : null,
      icon: running
          ? const SizedBox(
              width: 16,
              height: 16,
              child: CircularProgressIndicator(strokeWidth: 2))
          : Icon(_icon, size: 18),
      label: Text(feedbackActionLabel(action.wire)),
    );
  }
}

/// 截图：按需懒加载（带 Bearer），失败显示「附件不可用」占位。
class _FeedbackScreenshot extends ConsumerWidget {
  const _FeedbackScreenshot({required this.detail, required this.sourceId});

  final FeedbackDetail detail;
  final String sourceId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final api = ref.read(apiProvider);
    final uri =
        api.feedbackAttachmentUri(sourceId, detail.id, 'screenshot');
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
        appBar: AppBar(title: const Text('截图')),
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

/// 日志附件条目：点开预览文本（经反馈附件接口拉取）；可复制。
class _FeedbackLogTile extends ConsumerWidget {
  const _FeedbackLogTile({
    required this.detail,
    required this.log,
    required this.sourceId,
  });

  final FeedbackDetail detail;
  final FeedbackLogEntry log;
  final String sourceId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    return ListTile(
      leading: Icon(Icons.article_outlined,
          color: Theme.of(context).colorScheme.primary),
      title: Text(
          log.filename.isEmpty ? log.id : log.filename,
          maxLines: 1,
          overflow: TextOverflow.ellipsis),
      subtitle: Text(
          '${formatBytes(log.byteSize)}${log.source != null ? ' · ${log.source}' : ''}'),
      trailing: const Icon(Icons.chevron_right),
      onTap: () => showDialog<void>(
        context: context,
        builder: (_) => _FeedbackLogPreview(
          sourceId: sourceId,
          feedbackId: detail.id,
          log: log,
        ),
      ),
    );
  }
}

class _FeedbackLogPreview extends ConsumerStatefulWidget {
  const _FeedbackLogPreview({
    required this.sourceId,
    required this.feedbackId,
    required this.log,
  });

  final String sourceId;
  final String feedbackId;
  final FeedbackLogEntry log;

  @override
  ConsumerState<_FeedbackLogPreview> createState() =>
      _FeedbackLogPreviewState();
}

class _FeedbackLogPreviewState extends ConsumerState<_FeedbackLogPreview> {
  late Future<String> _future;

  @override
  void initState() {
    super.initState();
    _future = _load();
  }

  Future<String> _load() async {
    final api = ref.read(apiProvider);
    final res = await api.fetchFeedbackAttachment(
        widget.sourceId, widget.feedbackId, 'logs/${widget.log.id}');
    if (res.bytes.length > 256 * 1024) {
      return '（文件过大，仅显示前 256KiB）\n\n'
          '${utf8.decode(res.bytes.sublist(0, 256 * 1024), allowMalformed: true)}';
    }
    return utf8.decode(res.bytes, allowMalformed: true);
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Text(
          widget.log.filename.isEmpty ? widget.log.id : widget.log.filename),
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
                style:
                    const TextStyle(fontFamily: 'monospace', fontSize: 12),
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
                ScaffoldMessenger.of(context).showSnackBar(
                    const SnackBar(content: Text('已复制到剪贴板')));
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
