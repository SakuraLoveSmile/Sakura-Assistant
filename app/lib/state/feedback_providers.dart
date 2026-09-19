import 'dart:async';

import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../api/api_exception.dart';
import '../models/feedback.dart';
import 'providers.dart';

/// 反馈管理数据变更计数：操作成功 / 冲突 / 不确定后 bump → 列表重新拉取。
/// （详情就地用响应 `detail` 快照更新，不经此通道。）
final feedbackDataRevisionProvider =
    NotifierProvider<FeedbackDataRevision, int>(FeedbackDataRevision.new);

class FeedbackDataRevision extends Notifier<int> {
  @override
  int build() => 0;

  void bump() => state++;
}

// ---------------- 列表（游标分页 + 搜索 + 视图） ----------------

class FeedbackListQuery {
  const FeedbackListQuery({
    required this.sourceId,
    this.view = FeedbackView.inbox,
    this.q = '',
  });

  final String sourceId;
  final FeedbackView view;
  final String q;

  @override
  bool operator ==(Object other) =>
      other is FeedbackListQuery &&
      other.sourceId == sourceId &&
      other.view == view &&
      other.q == q;

  @override
  int get hashCode => Object.hash(sourceId, view, q);
}

class FeedbackListState {
  const FeedbackListState({
    required this.items,
    this.nextCursor,
    this.counts = const {},
    this.sourceName,
    this.loadingMore = false,
  });

  final List<FeedbackItem> items;
  final String? nextCursor;

  /// 三区计数（同一 q 下）：`{inbox, archived, trash}`。
  final Map<String, int> counts;
  final String? sourceName;
  final bool loadingMore;

  bool get hasMore => nextCursor != null;

  int? countOf(FeedbackView view) => counts[view.wire];

  FeedbackListState copyWith({
    List<FeedbackItem>? items,
    String? nextCursor,
    Map<String, int>? counts,
    String? sourceName,
    bool? loadingMore,
  }) =>
      FeedbackListState(
        items: items ?? this.items,
        nextCursor: nextCursor,
        counts: counts ?? this.counts,
        sourceName: sourceName ?? this.sourceName,
        loadingMore: loadingMore ?? this.loadingMore,
      );
}

final feedbackListProvider = AsyncNotifierProvider.family
    .autoDispose<FeedbackListNotifier, FeedbackListState, FeedbackListQuery>(
        FeedbackListNotifier.new);

class FeedbackListNotifier extends AsyncNotifier<FeedbackListState> {
  FeedbackListNotifier(this.query);

  final FeedbackListQuery query;

  @override
  Future<FeedbackListState> build() {
    // 操作成功 / 详情刷新后 bump → 重新拉取当前视图。
    ref.watch(feedbackDataRevisionProvider);
    return _fetch(null);
  }

  Future<FeedbackListState> _fetch(String? cursor) async {
    final page = await ref.read(apiProvider).getFeedbackList(
          sourceId: query.sourceId,
          view: query.view.wire,
          cursor: cursor,
          limit: 50,
          q: query.q.isEmpty ? null : query.q,
        );
    return FeedbackListState(
      items: page.items,
      nextCursor: page.nextCursor,
      counts: page.counts,
      sourceName: page.sourceName,
    );
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
      state = AsyncData(FeedbackListState(
        items: [...current.items, ...next.items],
        nextCursor: next.nextCursor,
        counts: next.counts,
        sourceName: next.sourceName,
      ));
    } on ApiException catch (e) {
      state = AsyncData(current.copyWith(loadingMore: false));
      if (e.isUnauthorized) rethrow;
    } catch (_) {
      state = AsyncData(current.copyWith(loadingMore: false));
    }
  }
}

// ---------------- 详情 + 管理操作 ----------------

/// 详情查询参数（来源 + 反馈 ID，命名空间隔离杜绝跨来源混淆）。
typedef FeedbackRef = ({String sourceId, String feedbackId});

/// 操作执行结果分类（页面据此给 snackbar / 提示）。
enum FeedbackActionStatus {
  /// 已执行（响应 detail 已就地刷新）。
  ok,

  /// 同 requestId 幂等回放（服务端未重复执行）。
  replayed,

  /// 版本 / 状态冲突：已用响应 detail 刷新。
  conflict,

  /// 结果待确认（超时 / 网络错 / outcome_uncertain / request_in_flight）：
  /// 已拉详情核对，绝不自动重发。
  uncertain,

  /// 可重试类（busy / 上游不可达 / 限流）：已拉详情核对。
  retryable,

  /// 需在「接入管理」配置（回连地址 / 管理凭证）。
  configNeeded,

  /// 需升级 Feedback 服务。
  upgradeNeeded,

  /// 对象不存在（含已 purge）。
  notFound,

  /// 其余失败。
  failed,
}

class FeedbackActionOutcome {
  const FeedbackActionOutcome(this.status, this.message);

  final FeedbackActionStatus status;
  final String message;
}

final feedbackDetailProvider = AsyncNotifierProvider.family
    .autoDispose<FeedbackDetailNotifier, FeedbackDetail, FeedbackRef>(
        FeedbackDetailNotifier.new);

class FeedbackDetailNotifier extends AsyncNotifier<FeedbackDetail> {
  FeedbackDetailNotifier(this.arg);

  final FeedbackRef arg;

  /// 单实例内防重入（页面另有按钮禁用态；此处兜底防重复提交）。
  bool _actionInFlight = false;

  /// 「结果待确认」动作的 requestId 留存：
  /// 契约允许**仅在用户显式重试同一未决动作**时复用同一 requestId
  /// （服务端幂等去重）；其余每次操作都必须生成新值。
  /// 一旦动作执行出确定结果（成功/冲突/其他错误）或动作种类变化即清除。
  ({FeedbackAction action, String requestId})? _uncertainPending;

  @override
  Future<FeedbackDetail> build() => ref.read(apiProvider).getFeedbackDetail(
        sourceId: arg.sourceId,
        feedbackId: arg.feedbackId,
      );

  Future<void> refresh() async {
    state = await AsyncValue.guard(() => ref
        .read(apiProvider)
        .getFeedbackDetail(sourceId: arg.sourceId, feedbackId: arg.feedbackId));
  }

  /// 静默拉详情核对（结果待确认路径）：失败不覆盖现有展示。
  Future<void> _refreshSilently() async {
    try {
      final d = await ref.read(apiProvider).getFeedbackDetail(
          sourceId: arg.sourceId, feedbackId: arg.feedbackId);
      state = AsyncData(d);
    } catch (_) {}
  }

  /// 执行管理操作（feedback-integration §4.2）。
  ///
  /// 语义：
  /// - 每次操作生成唯一 `requestId`；本端**绝不自动重发**；
  /// - 成功 / 幂等回放 → 用响应 `detail` 就地刷新 + 触发列表失效；
  /// - `version_conflict` / `revision_conflict` / `invalid_state` →
  ///   用响应 `detail` 刷新并提示；
  /// - 超时 / 网络错 / `outcome_uncertain` / `request_in_flight` →
  ///   「结果待确认」+ 自动拉详情核对；
  /// - `feedback_mgmt_unsupported` / `*_not_configured` → 只读态提示。
  Future<FeedbackActionOutcome> runAction(FeedbackAction action) async {
    final current = state.value;
    if (current == null) {
      return const FeedbackActionOutcome(
          FeedbackActionStatus.failed, '详情未加载，请刷新后重试');
    }
    if (_actionInFlight) {
      return const FeedbackActionOutcome(
          FeedbackActionStatus.retryable, '操作正在进行中，请稍候');
    }
    _actionInFlight = true;
    final api = ref.read(apiProvider);
    // 仅当这是对「同一未决动作」的显式重试时复用 requestId；
    // 否则每次操作生成新的幂等键。
    final pending = _uncertainPending;
    final requestId = (pending != null && pending.action == action)
        ? pending.requestId
        : generateFeedbackRequestId();
    try {
      final result = await api.postFeedbackAction(
        sourceId: arg.sourceId,
        feedbackId: arg.feedbackId,
        requestId: requestId,
        action: action.wire,
        expectedLifecycleVersion:
            action.isLifecycle ? current.lifecycleVersion : null,
        expectedRevision: action.needsRevision ? current.revision : null,
      );
      final detail = result.detail;
      if (detail != null) state = AsyncData(detail);
      _uncertainPending = null; // 已确定：清除待重试留存。
      ref.read(feedbackDataRevisionProvider.notifier).bump();
      return result.replayed
          ? const FeedbackActionOutcome(
              FeedbackActionStatus.replayed, '操作已完成（该请求此前已执行）')
          : const FeedbackActionOutcome(
              FeedbackActionStatus.ok, '操作已执行');
    } on ApiException catch (e) {
      // 冲突类响应携带 detail 快照：先就地刷新，避免展示过期状态。
      final snap = e.detail;
      if (snap != null && snap.isNotEmpty) {
        state = AsyncData(FeedbackDetail.fromJson(snap));
      }
      // 服务端返回了确定的错误结论 → 此前的未决动作视为已终结；
      // uncertain 分支会重新留存本次动作的 requestId。
      _uncertainPending = null;
      switch (e.feedbackCategory) {
        case FeedbackErrorCategory.conflict:
          ref.read(feedbackDataRevisionProvider.notifier).bump();
          return FeedbackActionOutcome(
              FeedbackActionStatus.conflict, '${e.message}（已刷新为最新状态）');
        case FeedbackErrorCategory.uncertain:
          // outcome_uncertain / request_in_flight：拉详情核对，绝不自动重发；
          // 留存 requestId 供用户显式重试同一动作时复用。
          _uncertainPending = (action: action, requestId: requestId);
          await _refreshSilently();
          ref.read(feedbackDataRevisionProvider.notifier).bump();
          return FeedbackActionOutcome(FeedbackActionStatus.uncertain,
              '结果待确认：${e.message}，已为你核对最新状态');
        case FeedbackErrorCategory.needsConfig:
          return FeedbackActionOutcome(
            FeedbackActionStatus.configNeeded,
            e.isFeedbackMgmtNotConfigured
                ? '该来源未配置管理凭证（mgmtKey），请在「接入管理」中配置'
                : '该来源未配置回连地址（attachmentBaseUrl），请在「接入管理」中配置',
          );
        case FeedbackErrorCategory.needsUpgrade:
          return const FeedbackActionOutcome(
              FeedbackActionStatus.upgradeNeeded,
              '当前 Feedback 服务不支持管理操作，需升级后使用');
        case FeedbackErrorCategory.retryable:
          // busy / 上游不可达 / 限流：拉详情核对当前状态后提示重试。
          await _refreshSilently();
          ref.read(feedbackDataRevisionProvider.notifier).bump();
          return FeedbackActionOutcome(
              FeedbackActionStatus.retryable, '${e.message}（已核对最新状态）');
        case FeedbackErrorCategory.other:
          if (e.isNotFound) {
            return const FeedbackActionOutcome(
                FeedbackActionStatus.notFound, '反馈不存在或已被彻底删除');
          }
          return FeedbackActionOutcome(FeedbackActionStatus.failed, e.message);
      }
    } on ApiNetworkException {
      // 网络错 / 超时：请求可能已到达服务端 —— 绝不自动重发，拉详情核对；
      // 留存 requestId 供用户显式重试同一动作时复用。
      _uncertainPending = (action: action, requestId: requestId);
      await _refreshSilently();
      ref.read(feedbackDataRevisionProvider.notifier).bump();
      return const FeedbackActionOutcome(
          FeedbackActionStatus.uncertain,
          '结果待确认：网络异常或超时，已为你核对最新状态；如未生效请稍后手动重试');
    } finally {
      _actionInFlight = false;
    }
  }
}
