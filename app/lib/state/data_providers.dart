import 'dart:async';

import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../api/api_client.dart';
import '../api/api_exception.dart';
import '../models/fault.dart';
import '../models/message.dart';
import '../models/metrics.dart';
import '../models/overview.dart';
import '../models/rules.dart';
import '../models/settings.dart';
import '../models/source.dart';
import 'providers.dart';

/// 首页一屏数据。失败时 error 态交给页面显示重试。
final overviewProvider =
    AsyncNotifierProvider<OverviewNotifier, Overview>(OverviewNotifier.new);

class OverviewNotifier extends AsyncNotifier<Overview> {
  @override
  Future<Overview> build() => ref.read(apiProvider).getOverview();

  Future<void> refresh() async {
    state = await AsyncValue.guard(() => ref.read(apiProvider).getOverview());
  }
}

// ---------------- 历史页（分页） ----------------

class HistoryQuery {
  const HistoryQuery({this.filter = MessageFilter.all, this.sourceId});

  final MessageFilter filter;
  final String? sourceId;

  @override
  bool operator ==(Object other) =>
      other is HistoryQuery &&
      other.filter == filter &&
      other.sourceId == sourceId;

  @override
  int get hashCode => Object.hash(filter, sourceId);
}

class HistoryState {
  const HistoryState({
    required this.items,
    this.nextCursor,
    this.offline = false,
    this.loadingMore = false,
  });

  final List<Message> items;
  final String? nextCursor;
  final bool offline;
  final bool loadingMore;

  bool get hasMore => offline ? false : nextCursor != null;

  HistoryState copyWith({
    List<Message>? items,
    String? nextCursor,
    bool? offline,
    bool? loadingMore,
  }) =>
      HistoryState(
        items: items ?? this.items,
        nextCursor: nextCursor,
        offline: offline ?? this.offline,
        loadingMore: loadingMore ?? this.loadingMore,
      );
}

final historyProvider = AsyncNotifierProvider.family
    .autoDispose<HistoryNotifier, HistoryState, HistoryQuery>(
        HistoryNotifier.new);

class HistoryNotifier extends AsyncNotifier<HistoryState> {
  HistoryNotifier(this.query);

  final HistoryQuery query;

  @override
  Future<HistoryState> build() async {
    ref.watch(cacheRevisionProvider);
    return _fetch(null);
  }

  Future<HistoryState> _fetch(String? cursor) async {
    final api = ref.read(apiProvider);
    try {
      final page = await api.getMessages(
        cursor: cursor,
        limit: 50,
        filter: query.filter,
        source: query.sourceId,
      );
      return HistoryState(items: page.items, nextCursor: page.nextCursor);
    } on ApiNetworkException {
      // 离线兜底：读本地缓存快照（历史页离线可读）。
      final cache = ref.read(cacheProvider);
      final items = await cache.messages(
        filter: query.filter.name,
        sourceId: query.sourceId,
        limit: 500,
      );
      return HistoryState(items: items, offline: true);
    }
  }

  Future<void> refresh() async {
    state = await AsyncValue.guard(() => _fetch(null));
  }

  Future<void> loadMore() async {
    final current = state.value;
    if (current == null || current.loadingMore || !current.hasMore) return;
    state = AsyncData(current.copyWith(loadingMore: true));
    try {
      final next = await _fetch(current.nextCursor);
      state = AsyncData(HistoryState(
        items: [...current.items, ...next.items],
        nextCursor: next.nextCursor,
        offline: next.offline,
      ));
    } on ApiException catch (e) {
      state = AsyncData(current.copyWith(loadingMore: false));
      if (e.isUnauthorized) rethrow;
    } catch (_) {
      state = AsyncData(current.copyWith(loadingMore: false));
    }
  }
}

// ---------------- 消息详情 ----------------

final messageDetailProvider = FutureProvider.autoDispose
    .family<MessageDetail, String>((ref, id) async {
  ref.watch(cacheRevisionProvider);
  final api = ref.read(apiProvider);
  try {
    return await api.getMessage(id);
  } on ApiNetworkException {
    final cache = ref.read(cacheProvider);
    final message = await cache.messageById(id);
    if (message == null) rethrow;
    final fault =
        message.faultId == null ? null : await cache.faultById(message.faultId!);
    return MessageDetail(message: message, fault: fault);
  }
});

// ---------------- 故障详情（契约无单故障端点：本地缓存 + 消息时间线推导） ----------------

class FaultDetailData {
  const FaultDetailData({required this.fault, required this.events});

  final Fault? fault;

  /// 全部相关消息，按 occurredAt 升序。
  final List<Message> events;

  /// 按轮次分组（incident 未知归入 0）。
  Map<int, List<Message>> get byIncident {
    final map = <int, List<Message>>{};
    for (final m in events) {
      map.putIfAbsent(m.incident ?? 0, () => []).add(m);
    }
    return map;
  }
}

final faultDetailProvider = FutureProvider.autoDispose
    .family<FaultDetailData, String>((ref, id) async {
  ref.watch(cacheRevisionProvider);
  final cache = ref.read(cacheProvider);
  var fault = await cache.faultById(id);
  var events = await cache.messagesForFault(id);
  if (fault == null) {
    // 缓存未命中（尚未 sync 或已被 tombstone 清掉的 resolved 故障）：
    // 尝试 REST 故障队列兜底。
    try {
      final api = ref.read(apiProvider);
      for (final state in ['open', 'resolved']) {
        String? cursor;
        var guard = 0;
        do {
          final page = await api.getFaults(state: state, cursor: cursor);
          for (final f in page.items) {
            if (f.id == id) fault = f;
          }
          cursor = page.nextCursor;
        } while (fault == null && cursor != null && ++guard < 10);
        if (fault != null) break;
      }
    } catch (_) {}
  }
  if (events.isEmpty && fault != null) {
    // 缓存无该故障消息：尝试在线拉故障消息页粗筛（REST 无按 fault 过滤参数）。
    try {
      final api = ref.read(apiProvider);
      String? cursor;
      var guard = 0;
      final found = <Message>[];
      do {
        final page =
            await api.getMessages(cursor: cursor, filter: MessageFilter.faults);
        found.addAll(page.items.where((m) => m.faultId == id));
        cursor = page.nextCursor;
      } while (cursor != null && ++guard < 10 && found.isEmpty);
      events = found;
    } catch (_) {}
  }
  return FaultDetailData(fault: fault, events: events);
});

// ---------------- 趋势 ----------------

class TrendsQuery {
  const TrendsQuery({
    required this.sourceId,
    required this.metric,
    this.step = 'raw',
    this.label,
  });

  final String sourceId;
  final String metric;
  final String step; // raw | 5m | 1h
  final String? label;

  @override
  bool operator ==(Object other) =>
      other is TrendsQuery &&
      other.sourceId == sourceId &&
      other.metric == metric &&
      other.step == step &&
      other.label == label;

  @override
  int get hashCode => Object.hash(sourceId, metric, step, label);
}

final trendsProvider = FutureProvider.autoDispose
    .family<List<MetricPoint>, TrendsQuery>((ref, query) {
  final to = DateTime.now().toUtc();
  // raw 看最近 6 小时，5m/1h 看最近 7 天。
  final from = query.step == 'raw'
      ? to.subtract(const Duration(hours: 6))
      : to.subtract(const Duration(days: 7));
  return ref.read(apiProvider).getMetricSeries(
        source: query.sourceId,
        metric: query.metric,
        from: from,
        to: to,
        step: query.step,
        label: query.label,
      );
});

// ---------------- 来源（管理 + 筛选器用） ----------------

final sourcesProvider =
    AsyncNotifierProvider<SourcesNotifier, List<Source>>(SourcesNotifier.new);

class SourcesNotifier extends AsyncNotifier<List<Source>> {
  @override
  Future<List<Source>> build() async {
    ref.watch(cacheRevisionProvider);
    try {
      return await ref.read(apiProvider).getSources();
    } on ApiNetworkException {
      return ref.read(cacheProvider).sources();
    }
  }

  Future<void> refresh() async {
    state = await AsyncValue.guard(() async {
      try {
        return await ref.read(apiProvider).getSources();
      } on ApiNetworkException {
        return ref.read(cacheProvider).sources();
      }
    });
  }
}

// ---------------- 规则 / 设置 ----------------

final rulesProvider = FutureProvider<Rules>((ref) async {
  ref.watch(cacheRevisionProvider);
  try {
    return await ref.read(apiProvider).getRules();
  } on ApiNetworkException {
    final cached = await ref.read(cacheProvider).rules();
    if (cached != null) return cached;
    rethrow;
  }
});

final settingsProvider = FutureProvider<Settings>((ref) async {
  ref.watch(cacheRevisionProvider);
  try {
    return await ref.read(apiProvider).getSettings();
  } on ApiNetworkException {
    final cached = await ref.read(cacheProvider).settings();
    if (cached != null) return cached;
    rethrow;
  }
});

/// 未读消息数（优先 overview，离线回退缓存统计）。
final unreadCountProvider = FutureProvider<int>((ref) async {
  ref.watch(cacheRevisionProvider);
  final overview = ref.watch(overviewProvider).value;
  if (overview != null) return overview.unreadMessages;
  try {
    return await ref.read(cacheProvider).unreadMessageCount();
  } catch (_) {
    return 0;
  }
});
