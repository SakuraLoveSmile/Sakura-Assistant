import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../api/api_exception.dart';
import '../models/feedback.dart';
import '../models/source.dart';
import '../state/data_providers.dart';
import '../state/feedback_providers.dart';
import '../state/providers.dart';
import '../widgets/common.dart';
import '../widgets/format.dart';

/// 反馈管理列表页（api-v1 §3.1）：
/// 来源选择（kind=feedback）+ 视图页签（收件箱/已归档/回收站 + counts）
/// + 搜索（q）+ 下拉刷新 + 游标分页。
class FeedbackListPage extends ConsumerStatefulWidget {
  const FeedbackListPage({super.key, this.initialSourceId});

  /// 路由 query `?source=` 预选来源。
  final String? initialSourceId;

  @override
  ConsumerState<FeedbackListPage> createState() => _FeedbackListPageState();
}

class _FeedbackListPageState extends ConsumerState<FeedbackListPage> {
  String? _sourceId;
  FeedbackView _view = FeedbackView.inbox;
  String _q = '';
  bool _searching = false;
  final _searchController = TextEditingController();
  Timer? _debounce;
  final _scroll = ScrollController();

  @override
  void initState() {
    super.initState();
    _sourceId = widget.initialSourceId;
    _scroll.addListener(_onScroll);
  }

  @override
  void dispose() {
    _debounce?.cancel();
    _searchController.dispose();
    _scroll.dispose();
    super.dispose();
  }

  FeedbackListQuery? get _query => _sourceId == null
      ? null
      : FeedbackListQuery(sourceId: _sourceId!, view: _view, q: _q);

  void _onScroll() {
    if (!_scroll.hasClients) return;
    final query = _query;
    if (query == null) return;
    if (_scroll.position.pixels >= _scroll.position.maxScrollExtent - 200) {
      ref.read(feedbackListProvider(query).notifier).loadMore();
    }
  }

  void _onSearchChanged(String text) {
    _debounce?.cancel();
    _debounce = Timer(const Duration(milliseconds: 350), () {
      final q = text.trim();
      if (q != _q && mounted) setState(() => _q = q);
    });
  }

  void _toggleSearch() {
    setState(() {
      _searching = !_searching;
      if (!_searching) {
        _searchController.clear();
        _debounce?.cancel();
        _q = '';
      }
    });
  }

  @override
  Widget build(BuildContext context) {
    final sourcesAsync = ref.watch(sourcesProvider);
    final feedbackSources = (sourcesAsync.value ?? const <Source>[])
        .where((s) => s.kind == 'feedback')
        .toList();
    // 自动选中：预选失效 / 未选时落到第一个 feedback 来源。
    if (feedbackSources.isNotEmpty &&
        (feedbackSources.every((s) => s.id != _sourceId))) {
      _sourceId = feedbackSources.first.id;
    }
    final query = _query;
    final state = query == null ? null : ref.watch(feedbackListProvider(query));
    final counts = state?.value?.counts ?? const <String, int>{};
    final online = ref.watch(onlineProvider).value ?? true;

    return Scaffold(
      appBar: AppBar(
        title: _searching
            ? TextField(
                controller: _searchController,
                autofocus: true,
                decoration: const InputDecoration(
                  hintText: '搜索标题 / 正文 / ID',
                  border: InputBorder.none,
                  filled: false,
                ),
                onChanged: _onSearchChanged,
              )
            : const Text('反馈管理'),
        actions: [
          IconButton(
            tooltip: _searching ? '关闭搜索' : '搜索',
            icon: Icon(_searching ? Icons.close : Icons.search),
            onPressed: _toggleSearch,
          ),
          IconButton(
            tooltip: '刷新',
            icon: const Icon(Icons.refresh),
            onPressed: query == null
                ? null
                : () =>
                    ref.read(feedbackListProvider(query).notifier).refresh(),
          ),
        ],
      ),
      body: Column(
        children: [
          if (!online)
            const StatusBanner(
              icon: Icons.wifi_off_outlined,
              text: '手机当前无网络连接，反馈管理需要在线访问',
              color: Color(0xFFE8960C),
            ),
          Padding(
            padding: const EdgeInsets.fromLTRB(16, 8, 16, 0),
            child: Row(
              children: [
                Expanded(
                  child: SegmentedButton<FeedbackView>(
                    segments: [
                      _viewSegment(FeedbackView.inbox, '收件箱', counts),
                      _viewSegment(FeedbackView.archived, '已归档', counts),
                      _viewSegment(FeedbackView.trash, '回收站', counts),
                    ],
                    selected: {_view},
                    onSelectionChanged: (s) => setState(() => _view = s.first),
                    style: const ButtonStyle(
                      visualDensity: VisualDensity.compact,
                    ),
                  ),
                ),
                if (feedbackSources.length > 1) ...[
                  const SizedBox(width: 8),
                  _SourcePicker(
                    sources: feedbackSources,
                    value: _sourceId,
                    onChanged: (v) => setState(() => _sourceId = v),
                  ),
                ],
              ],
            ),
          ),
          if (feedbackSources.length == 1)
            Padding(
              padding: const EdgeInsets.fromLTRB(16, 6, 16, 0),
              child: Row(
                children: [
                  Icon(Icons.forum_outlined,
                      size: 14, color: Theme.of(context).colorScheme.outline),
                  const SizedBox(width: 4),
                  Expanded(
                    child: Text(
                      feedbackSources.first.name,
                      style: Theme.of(context).textTheme.bodySmall?.copyWith(
                          color: Theme.of(context).colorScheme.outline),
                      overflow: TextOverflow.ellipsis,
                    ),
                  ),
                ],
              ),
            ),
          Expanded(child: _buildBody(query, state, sourcesAsync, feedbackSources)),
        ],
      ),
    );
  }

  ButtonSegment<FeedbackView> _viewSegment(
      FeedbackView view, String label, Map<String, int> counts) {
    final n = counts[view.wire];
    return ButtonSegment(
      value: view,
      label: Text(n == null ? label : '$label $n'),
    );
  }

  Widget _buildBody(
    FeedbackListQuery? query,
    AsyncValue<FeedbackListState>? state,
    AsyncValue<List<Source>> sourcesAsync,
    List<Source> feedbackSources,
  ) {
    if (sourcesAsync.hasError) {
      return ErrorView(
        error: sourcesAsync.error!,
        onRetry: () => ref.read(sourcesProvider.notifier).refresh(),
      );
    }
    if (sourcesAsync.isLoading && feedbackSources.isEmpty) {
      return const Center(child: CircularProgressIndicator());
    }
    if (feedbackSources.isEmpty) {
      return ListView(
        physics: const AlwaysScrollableScrollPhysics(),
        children: const [
          SizedBox(height: 160),
          EmptyView(
              icon: Icons.forum_outlined,
              text: '没有 Feedback 类来源，请先在「接入管理」创建'),
        ],
      );
    }
    if (query == null || state == null) {
      return const Center(child: CircularProgressIndicator());
    }
    return RefreshIndicator(
      onRefresh: () => ref.read(feedbackListProvider(query).notifier).refresh(),
      child: AsyncValueView(
        value: state,
        onRetry: () =>
            ref.read(feedbackListProvider(query).notifier).refresh(),
        errorBuilder: (error) => _errorView(error, query),
        builder: (data) {
          if (data.items.isEmpty) {
            return ListView(
              physics: const AlwaysScrollableScrollPhysics(),
              children: [
                const SizedBox(height: 160),
                EmptyView(
                  icon: Icons.inbox_outlined,
                  text: _q.isNotEmpty
                      ? '没有匹配「$_q」的反馈'
                      : '${_viewLabel(_view)}里没有反馈',
                ),
              ],
            );
          }
          return ListView.builder(
            controller: _scroll,
            physics: const AlwaysScrollableScrollPhysics(),
            padding: const EdgeInsets.only(bottom: 32),
            itemCount: data.items.length + (data.hasMore ? 1 : 0),
            itemBuilder: (context, index) {
              if (index >= data.items.length) {
                return const Padding(
                  padding: EdgeInsets.all(16),
                  child: Center(child: CircularProgressIndicator()),
                );
              }
              final item = data.items[index];
              return _FeedbackItemTile(
                item: item,
                onTap: () => context.push(
                  '/sources/${Uri.encodeComponent(query.sourceId)}'
                  '/feedback/${Uri.encodeComponent(item.id)}',
                ),
              );
            },
          );
        },
      ),
    );
  }

  String _viewLabel(FeedbackView v) => switch (v) {
        FeedbackView.inbox => '收件箱',
        FeedbackView.archived => '已归档',
        FeedbackView.trash => '回收站',
        FeedbackView.all => '全部',
      };

  /// 列表错误态按反馈错误分类展示（api-v1 §3.1 前置判定）。
  Widget _errorView(Object error, FeedbackListQuery query) {
    final theme = Theme.of(context);
    if (error is ApiException) {
      switch (error.feedbackCategory) {
        case FeedbackErrorCategory.needsConfig:
          return _GuideErrorView(
            icon: Icons.vpn_key_off_outlined,
            title: error.isFeedbackMgmtNotConfigured
                ? '该来源未配置管理凭证'
                : '该来源未配置回连地址',
            detail: error.isFeedbackMgmtNotConfigured
                ? '反馈管理需要管理凭证（mgmtKey），请到「接入管理」为该来源轮换并下发管理密钥。'
                : '请先在「接入管理」为该来源配置 attachmentBaseUrl。',
            actionLabel: '去接入管理',
            onAction: () => context.push('/sources'),
            onRetry: () =>
                ref.read(feedbackListProvider(query).notifier).refresh(),
          );
        case FeedbackErrorCategory.needsUpgrade:
          return _GuideErrorView(
            icon: Icons.system_update_alt_outlined,
            title: '需升级 Feedback 服务',
            detail: '当前服务端版本过旧、没有管理面（v0.6.0 起提供）。'
                '升级前仍可在消息详情查看反馈内容与附件。',
            onRetry: () =>
                ref.read(feedbackListProvider(query).notifier).refresh(),
          );
        case FeedbackErrorCategory.uncertain:
          return _GuideErrorView(
            icon: Icons.help_outline,
            title: '结果待确认',
            detail: error.message,
            onRetry: () =>
                ref.read(feedbackListProvider(query).notifier).refresh(),
          );
        case FeedbackErrorCategory.retryable:
        case FeedbackErrorCategory.conflict:
        case FeedbackErrorCategory.other:
          return ListView(
            physics: const AlwaysScrollableScrollPhysics(),
            children: [
              SizedBox(
                height: 320,
                child: ErrorView(
                  error: error.isFeedbackUnavailable
                      ? '反馈服务不可达：${error.message}'
                      : error,
                  onRetry: () => ref
                      .read(feedbackListProvider(query).notifier)
                      .refresh(),
                ),
              ),
            ],
          );
      }
    }
    return ListView(
      physics: const AlwaysScrollableScrollPhysics(),
      children: [
        SizedBox(
          height: 320,
          child: Center(
            child: Text('加载失败：$error',
                style: theme.textTheme.bodyMedium,
                textAlign: TextAlign.center),
          ),
        ),
      ],
    );
  }
}

class _SourcePicker extends StatelessWidget {
  const _SourcePicker({
    required this.sources,
    required this.value,
    required this.onChanged,
  });

  final List<Source> sources;
  final String? value;
  final ValueChanged<String?> onChanged;

  @override
  Widget build(BuildContext context) {
    return DropdownButtonHideUnderline(
      child: DropdownButton<String>(
        value: value,
        hint: const Text('来源'),
        items: [
          for (final s in sources)
            DropdownMenuItem(
              value: s.id,
              child: Text(s.name, overflow: TextOverflow.ellipsis),
            ),
        ],
        onChanged: onChanged,
      ),
    );
  }
}

/// 引导型错误视图（需配置 / 需升级 / 待确认）。
class _GuideErrorView extends StatelessWidget {
  const _GuideErrorView({
    required this.icon,
    required this.title,
    required this.detail,
    this.actionLabel,
    this.onAction,
    this.onRetry,
  });

  final IconData icon;
  final String title;
  final String detail;
  final String? actionLabel;
  final VoidCallback? onAction;
  final VoidCallback? onRetry;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    return ListView(
      physics: const AlwaysScrollableScrollPhysics(),
      children: [
        Padding(
          padding: const EdgeInsets.fromLTRB(32, 80, 32, 0),
          child: Column(
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
              if (onAction != null && actionLabel != null)
                FilledButton.tonalIcon(
                  onPressed: onAction,
                  icon: const Icon(Icons.lan_outlined),
                  label: Text(actionLabel!),
                ),
              if (onRetry != null)
                TextButton.icon(
                  onPressed: onRetry,
                  icon: const Icon(Icons.refresh),
                  label: const Text('重试'),
                ),
            ],
          ),
        ),
      ],
    );
  }
}

/// 列表条目：状态 / mgmtState 徽标 / 错误摘要徽标 / 附件指示。
class _FeedbackItemTile extends StatelessWidget {
  const _FeedbackItemTile({required this.item, this.onTap});

  final FeedbackItem item;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final scheme = theme.colorScheme;
    final statusColor = feedbackStatusColor(item.status, scheme);
    final hasError = item.errorSummary != null && item.errorSummary!.isNotEmpty;
    return Card(
      margin: const EdgeInsets.fromLTRB(16, 6, 16, 0),
      child: InkWell(
        onTap: onTap,
        child: Padding(
          padding: const EdgeInsets.fromLTRB(16, 12, 16, 12),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Row(
                children: [
                  Container(
                    width: 8,
                    height: 8,
                    decoration: BoxDecoration(
                        color: statusColor, shape: BoxShape.circle),
                  ),
                  const SizedBox(width: 8),
                  Expanded(
                    child: Text(
                      item.displayTitle,
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                      style: theme.textTheme.titleSmall,
                    ),
                  ),
                  if (item.mgmtState != null) ...[
                    const SizedBox(width: 8),
                    _Chip(label: feedbackMgmtLabel(item.mgmtState)),
                  ],
                ],
              ),
              const SizedBox(height: 4),
              Text(
                '${feedbackStatusLabel(item.status)}'
                '${item.issueStatus != null ? ' · ${feedbackIssueLabel(item.issueStatus)}' : ''}'
                ' · ${formatRelative(item.updatedAt ?? item.createdAt)}'
                '${item.hasScreenshot ? ' · 截图' : ''}'
                '${item.logCount > 0 ? ' · 日志×${item.logCount}' : ''}'
                '${item.resumePaused ? ' · 已暂停' : ''}',
                maxLines: 1,
                overflow: TextOverflow.ellipsis,
                style: theme.textTheme.bodySmall
                    ?.copyWith(color: scheme.outline),
              ),
              if (hasError) ...[
                const SizedBox(height: 6),
                Container(
                  padding:
                      const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
                  decoration: BoxDecoration(
                    color: scheme.errorContainer.withValues(alpha: 0.5),
                    borderRadius: BorderRadius.circular(6),
                  ),
                  child: Row(
                    children: [
                      Icon(Icons.error_outline,
                          size: 14, color: scheme.onErrorContainer),
                      const SizedBox(width: 6),
                      Expanded(
                        child: Text(
                          item.errorSummary!,
                          maxLines: 2,
                          overflow: TextOverflow.ellipsis,
                          style: theme.textTheme.bodySmall
                              ?.copyWith(color: scheme.onErrorContainer),
                        ),
                      ),
                    ],
                  ),
                ),
              ],
            ],
          ),
        ),
      ),
    );
  }
}

class _Chip extends StatelessWidget {
  const _Chip({required this.label});

  final String label;

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
      decoration: BoxDecoration(
        color: scheme.surfaceContainerHighest,
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(label,
          style: TextStyle(fontSize: 11, color: scheme.outline)),
    );
  }
}
