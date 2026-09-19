import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../api/api_client.dart';
import '../state/data_providers.dart';
import '../widgets/common.dart';
import '../widgets/tiles.dart';

/// 历史页：filter（all/unread/feedback/faults）+ source 筛选 + 分页加载。
class HistoryPage extends ConsumerStatefulWidget {
  const HistoryPage({super.key, this.filter, this.sourceId});

  /// 路由 query 传入的初始 filter（all/unread/feedback/faults）。
  final String? filter;
  final String? sourceId;

  @override
  ConsumerState<HistoryPage> createState() => _HistoryPageState();
}

class _HistoryPageState extends ConsumerState<HistoryPage> {
  late MessageFilter _filter;
  String? _sourceId;
  final _scroll = ScrollController();

  @override
  void initState() {
    super.initState();
    _filter = MessageFilter.values.firstWhere(
      (f) => f.name == widget.filter,
      orElse: () => MessageFilter.all,
    );
    _sourceId = widget.sourceId;
    _scroll.addListener(_onScroll);
  }

  @override
  void dispose() {
    _scroll.dispose();
    super.dispose();
  }

  HistoryQuery get _query =>
      HistoryQuery(filter: _filter, sourceId: _sourceId);

  void _onScroll() {
    if (!_scroll.hasClients) return;
    if (_scroll.position.pixels >= _scroll.position.maxScrollExtent - 200) {
      ref.read(historyProvider(_query).notifier).loadMore();
    }
  }

  @override
  Widget build(BuildContext context) {
    final state = ref.watch(historyProvider(_query));
    final sources = ref.watch(sourcesProvider).value ?? const [];
    return Scaffold(
      appBar: AppBar(title: const Text('历史消息')),
      body: Column(
        children: [
          Padding(
            padding: const EdgeInsets.fromLTRB(16, 8, 16, 0),
            child: Row(
              children: [
                Expanded(
                  child: SegmentedButton<MessageFilter>(
                    segments: const [
                      ButtonSegment(value: MessageFilter.all, label: Text('全部')),
                      ButtonSegment(
                          value: MessageFilter.unread, label: Text('未读')),
                      ButtonSegment(
                          value: MessageFilter.feedback, label: Text('反馈')),
                      ButtonSegment(
                          value: MessageFilter.faults, label: Text('故障')),
                    ],
                    selected: {_filter},
                    onSelectionChanged: (s) =>
                        setState(() => _filter = s.first),
                    style: const ButtonStyle(
                      visualDensity: VisualDensity.compact,
                    ),
                  ),
                ),
                const SizedBox(width: 8),
                _SourceFilter(
                  sources: sources.map((s) => (id: s.id, name: s.name)).toList(),
                  value: _sourceId,
                  onChanged: (v) => setState(() => _sourceId = v),
                ),
              ],
            ),
          ),
          if (state.value?.offline ?? false)
            const StatusBanner(
              icon: Icons.offline_pin_outlined,
              text: '离线模式：显示本地缓存消息',
              color: Color(0xFFE8960C),
            ),
          Expanded(
            child: RefreshIndicator(
              onRefresh: () =>
                  ref.read(historyProvider(_query).notifier).refresh(),
              child: AsyncValueView(
                value: state,
                onRetry: () =>
                    ref.read(historyProvider(_query).notifier).refresh(),
                builder: (data) {
                  if (data.items.isEmpty) {
                    return ListView(
                      physics: const AlwaysScrollableScrollPhysics(),
                      children: const [
                        SizedBox(height: 160),
                        EmptyView(
                            icon: Icons.inbox_outlined, text: '没有符合条件的消息'),
                      ],
                    );
                  }
                  return ListView.builder(
                    controller: _scroll,
                    physics: const AlwaysScrollableScrollPhysics(),
                    itemCount: data.items.length + (data.hasMore ? 1 : 0),
                    itemBuilder: (context, index) {
                      if (index >= data.items.length) {
                        return const Padding(
                          padding: EdgeInsets.all(16),
                          child:
                              Center(child: CircularProgressIndicator()),
                        );
                      }
                      final m = data.items[index];
                      return MessageTile(
                        message: m,
                        onTap: () => context
                            .push('/messages/${Uri.encodeComponent(m.id)}'),
                      );
                    },
                  );
                },
              ),
            ),
          ),
        ],
      ),
    );
  }
}

class _SourceFilter extends StatelessWidget {
  const _SourceFilter({
    required this.sources,
    required this.value,
    required this.onChanged,
  });

  final List<({String id, String name})> sources;
  final String? value;
  final ValueChanged<String?> onChanged;

  @override
  Widget build(BuildContext context) {
    return DropdownButtonHideUnderline(
      child: DropdownButton<String>(
        value: value ?? '',
        hint: const Text('来源'),
        items: [
          const DropdownMenuItem(value: '', child: Text('全部来源')),
          for (final s in sources)
            DropdownMenuItem(value: s.id, child: Text(s.name)),
        ],
        onChanged: (v) => onChanged(v == '' ? null : v),
      ),
    );
  }
}
