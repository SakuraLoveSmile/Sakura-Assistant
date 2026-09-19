import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../api/api_client.dart';
import '../api/api_exception.dart';
import '../models/source.dart';
import '../state/data_providers.dart';
import '../state/providers.dart';
import '../widgets/common.dart';
import '../widgets/format.dart';

/// 接入管理：来源列表 / 创建（一次性展示 accessKey+安装命令）/
/// 重命名 / 启停 / 轮换密钥（一次性展示）/ 删除。
class SourcesPage extends ConsumerWidget {
  const SourcesPage({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final sources = ref.watch(sourcesProvider);
    return Scaffold(
      appBar: AppBar(
        title: const Text('接入管理'),
        actions: [
          IconButton(
            tooltip: '刷新',
            icon: const Icon(Icons.refresh),
            onPressed: () =>
                ref.read(sourcesProvider.notifier).refresh(),
          ),
        ],
      ),
      floatingActionButton: FloatingActionButton.extended(
        onPressed: () => _createSource(context, ref),
        icon: const Icon(Icons.add),
        label: const Text('新建来源'),
      ),
      body: AsyncValueView(
        value: sources,
        onRetry: () => ref.read(sourcesProvider.notifier).refresh(),
        builder: (items) {
          if (items.isEmpty) {
            return const EmptyView(
                icon: Icons.lan_outlined, text: '还没有接入来源，点右下角创建');
          }
          return RefreshIndicator(
            onRefresh: () => ref.read(sourcesProvider.notifier).refresh(),
            child: ListView.builder(
              physics: const AlwaysScrollableScrollPhysics(),
              padding: const EdgeInsets.only(bottom: 96),
              itemCount: items.length,
              itemBuilder: (context, i) => _SourceTile(source: items[i]),
            ),
          );
        },
      ),
    );
  }

  Future<void> _createSource(BuildContext context, WidgetRef ref) async {
    final result = await showDialog<CreatedSource>(
      context: context,
      builder: (_) => const _CreateSourceDialog(),
    );
    if (result == null) return;
    await ref.read(sourcesProvider.notifier).refresh();
    if (context.mounted) {
      await showDialog<void>(
        context: context,
        builder: (_) => _AccessKeyDialog(
          title: '来源已创建',
          accessKey: result.accessKey,
          install: result.install,
        ),
      );
    }
  }
}

class _SourceTile extends ConsumerWidget {
  const _SourceTile({required this.source});

  final Source source;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final scheme = Theme.of(context).colorScheme;
    return Card(
      margin: const EdgeInsets.fromLTRB(16, 8, 16, 0),
      child: ExpansionTile(
        leading: Icon(
          source.isDevice ? Icons.dns_outlined : Icons.forum_outlined,
          color: source.enabled ? scheme.primary : scheme.outline,
        ),
        title: Row(
          children: [
            Expanded(
              child: Text(source.name, overflow: TextOverflow.ellipsis),
            ),
            if (!source.enabled)
              Container(
                padding:
                    const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
                decoration: BoxDecoration(
                  color: scheme.outlineVariant,
                  borderRadius: BorderRadius.circular(4),
                ),
                child: const Text('已停用', style: TextStyle(fontSize: 11)),
              ),
          ],
        ),
        subtitle: Text(
          '${source.isDevice ? '设备采集' : 'Feedback 接入'} · '
          '${source.isOnline ? '在线' : '离线'} · '
          '${source.lastSeenAt != null ? '最后上报 ${formatRelative(source.lastSeenAt)}' : '从未上报'}'
          '${source.keyHint != null ? ' · 密钥尾号 ${source.keyHint}' : ''}'
          // v1.1：管理凭证末位回显（只显示 hint，绝不暴露 mgmtKey 本体）。
          '${source.mgmtKeyHint != null ? ' · 管理密钥尾号 ${source.mgmtKeyHint}' : (source.kind == 'feedback' ? ' · 未配置管理密钥' : '')}',
          style: Theme.of(context).textTheme.bodySmall,
        ),
        children: [
          if (source.attachmentBaseUrl != null)
            ListTile(
              dense: true,
              leading: const Icon(Icons.link, size: 18),
              title: Text('附件回连 ${source.attachmentBaseUrl}',
                  style: Theme.of(context).textTheme.bodySmall),
            ),
          OverflowBar(
            alignment: MainAxisAlignment.start,
            children: [
              if (source.kind == 'feedback')
                TextButton.icon(
                  icon: const Icon(Icons.forum_outlined, size: 18),
                  label: const Text('反馈管理'),
                  onPressed: () => context.push(
                      '/feedback?source=${Uri.encodeComponent(source.id)}'),
                ),
              TextButton.icon(
                icon: const Icon(Icons.edit_outlined, size: 18),
                label: const Text('重命名'),
                onPressed: () => _rename(context, ref),
              ),
              TextButton.icon(
                icon: Icon(
                    source.enabled
                        ? Icons.pause_circle_outline
                        : Icons.play_circle_outline,
                    size: 18),
                label: Text(source.enabled ? '停用' : '启用'),
                onPressed: () => _toggleEnabled(context, ref),
              ),
              TextButton.icon(
                icon: const Icon(Icons.key_outlined, size: 18),
                label: const Text('轮换密钥'),
                onPressed: () => _rotateKey(context, ref),
              ),
              TextButton.icon(
                icon: Icon(Icons.delete_outline,
                    size: 18, color: scheme.error),
                label: Text('删除', style: TextStyle(color: scheme.error)),
                onPressed: () => _delete(context, ref),
              ),
            ],
          ),
        ],
      ),
    );
  }

  Future<void> _rename(BuildContext context, WidgetRef ref) async {
    final controller = TextEditingController(text: source.name);
    final name = await showDialog<String>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('重命名来源'),
        content: TextField(
          controller: controller,
          autofocus: true,
          decoration: const InputDecoration(labelText: '名称'),
          onSubmitted: (_) => Navigator.of(ctx).pop(controller.text.trim()),
        ),
        actions: [
          TextButton(
              onPressed: () => Navigator.of(ctx).pop(),
              child: const Text('取消')),
          FilledButton(
              onPressed: () => Navigator.of(ctx).pop(controller.text.trim()),
              child: const Text('保存')),
        ],
      ),
    );
    if (name == null || name.isEmpty || name == source.name) return;
    if (!context.mounted) return;
    await _run(context, ref, () async {
      final updated = await ref
          .read(apiProvider)
          .updateSource(source.id, name: name);
      await ref.read(cacheProvider).upsertSource(updated);
      await ref.read(sourcesProvider.notifier).refresh();
    });
  }

  Future<void> _toggleEnabled(BuildContext context, WidgetRef ref) async {
    await _run(context, ref, () async {
      final updated = await ref
          .read(apiProvider)
          .updateSource(source.id, enabled: !source.enabled);
      await ref.read(cacheProvider).upsertSource(updated);
      await ref.read(sourcesProvider.notifier).refresh();
    });
  }

  Future<void> _rotateKey(BuildContext context, WidgetRef ref) async {
    final ok = await _confirm(
      context,
      '轮换密钥',
      '旧密钥将立即失效（来源事件流保留）。确定轮换「${source.name}」的密钥？',
    );
    if (!ok) return;
    if (!context.mounted) return;
    RotatedKey? rotated;
    await _run(context, ref, () async {
      rotated = await ref.read(apiProvider).rotateSourceKey(source.id);
      await ref.read(sourcesProvider.notifier).refresh();
    });
    if (rotated != null && context.mounted) {
      await showDialog<void>(
        context: context,
        builder: (_) => _AccessKeyDialog(
          title: '密钥已轮换',
          accessKey: rotated!.accessKey,
          install: rotated!.install,
        ),
      );
    }
  }

  Future<void> _delete(BuildContext context, WidgetRef ref) async {
    final ok = await _confirm(
      context,
      '删除来源',
      '将吊销密钥；指标 / 消息 / 故障历史保留。确定删除「${source.name}」？',
    );
    if (!ok) return;
    if (!context.mounted) return;
    await _run(context, ref, () async {
      await ref.read(apiProvider).deleteSource(source.id);
      await ref.read(cacheProvider).deleteSource(source.id);
      await ref.read(sourcesProvider.notifier).refresh();
    });
  }

  Future<bool> _confirm(BuildContext context, String title, String body) async {
    final result = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: Text(title),
        content: Text(body),
        actions: [
          TextButton(
              onPressed: () => Navigator.of(ctx).pop(false),
              child: const Text('取消')),
          FilledButton(
              onPressed: () => Navigator.of(ctx).pop(true),
              child: const Text('确定')),
        ],
      ),
    );
    return result ?? false;
  }

  Future<void> _run(BuildContext context, WidgetRef ref,
      Future<void> Function() action) async {
    try {
      await action();
      ref.read(cacheRevisionProvider.notifier).bump();
    } on ApiException catch (e) {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text('操作失败：${e.message}')));
      }
    } on ApiNetworkException {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(const SnackBar(content: Text('无法连接中枢')));
      }
    }
  }
}

/// 创建来源对话框：name + kind + 可选 attachmentBaseUrl。
class _CreateSourceDialog extends ConsumerStatefulWidget {
  const _CreateSourceDialog();

  @override
  ConsumerState<_CreateSourceDialog> createState() =>
      _CreateSourceDialogState();
}

class _CreateSourceDialogState extends ConsumerState<_CreateSourceDialog> {
  final _nameController = TextEditingController();
  final _baseUrlController = TextEditingController();
  String _kind = 'device';
  bool _submitting = false;
  String? _error;

  @override
  void dispose() {
    _nameController.dispose();
    _baseUrlController.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    final name = _nameController.text.trim();
    if (name.isEmpty) {
      setState(() => _error = '请输入名称');
      return;
    }
    setState(() {
      _submitting = true;
      _error = null;
    });
    try {
      final created = await ref.read(apiProvider).createSource(
            name: name,
            kind: _kind,
            attachmentBaseUrl: _baseUrlController.text.trim().isEmpty
                ? null
                : _baseUrlController.text.trim(),
          );
      if (mounted) Navigator.of(context).pop(created);
    } on ApiException catch (e) {
      setState(() {
        _error = e.message;
        _submitting = false;
      });
    } on ApiNetworkException {
      setState(() {
        _error = '无法连接中枢';
        _submitting = false;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: const Text('新建来源'),
      content: SizedBox(
        width: 420,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            TextField(
              controller: _nameController,
              autofocus: true,
              decoration: const InputDecoration(
                labelText: '名称',
                hintText: '如：飞牛 NAS / Feedback',
              ),
            ),
            const SizedBox(height: 12),
            SegmentedButton<String>(
              segments: const [
                ButtonSegment(
                    value: 'device',
                    label: Text('设备采集'),
                    icon: Icon(Icons.dns_outlined)),
                ButtonSegment(
                    value: 'feedback',
                    label: Text('Feedback'),
                    icon: Icon(Icons.forum_outlined)),
              ],
              selected: {_kind},
              onSelectionChanged: (s) => setState(() => _kind = s.first),
            ),
            const SizedBox(height: 12),
            TextField(
              controller: _baseUrlController,
              decoration: const InputDecoration(
                labelText: '附件回连地址（可选）',
                hintText: 'http://nas.local:8787，feedback 类来源用',
              ),
              keyboardType: TextInputType.url,
            ),
            if (_error != null) ...[
              const SizedBox(height: 12),
              Text(_error!,
                  style: TextStyle(
                      color: Theme.of(context).colorScheme.error)),
            ],
          ],
        ),
      ),
      actions: [
        TextButton(
            onPressed: () => Navigator.of(context).pop(),
            child: const Text('取消')),
        FilledButton(
          onPressed: _submitting ? null : _submit,
          child: _submitting
              ? const SizedBox(
                  width: 16,
                  height: 16,
                  child: CircularProgressIndicator(strokeWidth: 2))
              : const Text('创建'),
        ),
      ],
    );
  }
}

/// 一次性展示 accessKey + install command 的对话框（「仅显示一次」提示）。
class _AccessKeyDialog extends StatelessWidget {
  const _AccessKeyDialog({
    required this.title,
    required this.accessKey,
    this.install,
  });

  final String title;
  final String accessKey;
  final InstallInfo? install;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final scheme = theme.colorScheme;
    return AlertDialog(
      title: Text(title),
      content: SizedBox(
        width: 480,
        child: SingleChildScrollView(
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Container(
                padding: const EdgeInsets.all(10),
                decoration: BoxDecoration(
                  color: scheme.errorContainer.withValues(alpha: 0.4),
                  borderRadius: BorderRadius.circular(8),
                ),
                child: Row(
                  children: [
                    Icon(Icons.warning_amber_rounded,
                        size: 18, color: scheme.error),
                    const SizedBox(width: 8),
                    const Expanded(
                      child: Text('密钥仅显示一次，请立即复制保存，关闭后无法再次查看。',
                          style: TextStyle(fontSize: 13)),
                    ),
                  ],
                ),
              ),
              const SizedBox(height: 16),
              Text('访问密钥', style: theme.textTheme.titleSmall),
              const SizedBox(height: 6),
              _CopyRow(text: accessKey),
              if (install != null) ...[
                const SizedBox(height: 16),
                Text('安装命令', style: theme.textTheme.titleSmall),
                const SizedBox(height: 6),
                _CopyRow(text: install!.command, monospace: true),
                if (install!.note != null && install!.note!.isNotEmpty) ...[
                  const SizedBox(height: 12),
                  Text(install!.note!,
                      style: theme.textTheme.bodySmall
                          ?.copyWith(color: scheme.outline)),
                ],
              ],
            ],
          ),
        ),
      ),
      actions: [
        FilledButton(
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('我已保存'),
        ),
      ],
    );
  }
}

class _CopyRow extends StatelessWidget {
  const _CopyRow({required this.text, this.monospace = false});

  final String text;
  final bool monospace;

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 8),
      decoration: BoxDecoration(
        color: Theme.of(context).colorScheme.surfaceContainerHighest,
        borderRadius: BorderRadius.circular(8),
      ),
      child: Row(
        children: [
          Expanded(
            child: SelectableText(
              text,
              style: TextStyle(
                fontSize: 12,
                fontFamily: monospace ? 'monospace' : null,
              ),
            ),
          ),
          IconButton(
            icon: const Icon(Icons.copy_outlined, size: 18),
            tooltip: '复制',
            onPressed: () async {
              await Clipboard.setData(ClipboardData(text: text));
              if (context.mounted) {
                ScaffoldMessenger.of(context)
                    .showSnackBar(const SnackBar(content: Text('已复制')));
              }
            },
          ),
        ],
      ),
    );
  }
}
