import 'dart:convert';

import '../models/json.dart';

/// 反馈管理错误的处理分类（api-v1 §3.1 / feedback-integration §4.2）：
/// 客户端按分类决定「重试 / 引导配置 / 提示升级 / 拉详情核对」。
enum FeedbackErrorCategory {
  /// 可刷新重试：上游不可达 / 忙 / 限流。
  retryable,

  /// 需升级 Feedback 服务（旧版无管理面）。
  needsUpgrade,

  /// 需在「接入管理」配置（回连地址 / 管理凭证）。
  needsConfig,

  /// 结果待确认：拉详情核对，绝不自动重发。
  uncertain,

  /// 版本 / 状态冲突：响应携带 `detail` 快照供就地刷新。
  conflict,

  /// 其余错误：按 message 展示。
  other,
}

/// 中枢错误信封：`{ "error": { "code": "…", "message": "…" } }`。
class ApiException implements Exception {
  const ApiException({
    required this.code,
    required this.message,
    this.statusCode,
    this.version,
    this.retryAfterSeconds,
    this.detail,
  });

  final String code;
  final String message;
  final int? statusCode;

  /// `version_conflict` 时响应携带的当前版本号（位置契约未明确，宽容解析：
  /// error 块内与顶层都尝试）。
  final int? version;

  /// `rate_limited` 时 Retry-After 秒数。
  final int? retryAfterSeconds;

  /// 冲突类响应携带的当前对象快照（v1.1 反馈管理：`version_conflict` /
  /// `revision_conflict` / `invalid_state` 透传上游 `detail` 完整对象，
  /// 客户端据此就地刷新）。
  final Map<String, dynamic>? detail;

  bool get isUnauthorized => code == 'unauthorized' || statusCode == 401;
  bool get isVersionConflict => code == 'version_conflict';
  bool get isRateLimited => code == 'rate_limited';
  bool get isAttachmentUnavailable => code == 'attachment_unavailable';
  bool get isNotFound => code == 'not_found' || statusCode == 404;

  // ---- 反馈管理代理错误码（api-v1 §3.1，v1.1） ----
  bool get isFeedbackUnavailable => code == 'feedback_unavailable';
  bool get isFeedbackUpstreamNotConfigured =>
      code == 'feedback_upstream_not_configured';
  bool get isFeedbackMgmtNotConfigured =>
      code == 'feedback_mgmt_not_configured';
  bool get isFeedbackMgmtUnsupported => code == 'feedback_mgmt_unsupported';
  bool get isRevisionConflict => code == 'revision_conflict';
  bool get isInvalidState => code == 'invalid_state';
  bool get isBusy => code == 'busy';
  bool get isRequestIdConflict => code == 'request_id_conflict';
  bool get isRequestInFlight => code == 'request_in_flight';
  bool get isOutcomeUncertain => code == 'outcome_uncertain';

  /// 反馈管理端点错误的处理分类。
  FeedbackErrorCategory get feedbackCategory => switch (code) {
        'feedback_unavailable' ||
        'busy' ||
        'rate_limited' =>
          FeedbackErrorCategory.retryable,
        'feedback_mgmt_unsupported' => FeedbackErrorCategory.needsUpgrade,
        'feedback_mgmt_not_configured' ||
        'feedback_upstream_not_configured' =>
          FeedbackErrorCategory.needsConfig,
        // 进行中 / 结果不确定 / 超时：拉详情核对，绝不自动重发。
        'outcome_uncertain' ||
        'request_in_flight' =>
          FeedbackErrorCategory.uncertain,
        // 冲突类响应携带 detail 快照。
        'version_conflict' ||
        'revision_conflict' ||
        'invalid_state' =>
          FeedbackErrorCategory.conflict,
        _ => FeedbackErrorCategory.other,
      };

  /// 从响应体解析错误信封；解析不出给兜底文案。
  factory ApiException.fromBody(
    int statusCode,
    String body, {
    Map<String, String>? headers,
  }) {
    var code = switch (statusCode) {
      400 => 'invalid_request',
      401 => 'unauthorized',
      403 => 'forbidden',
      404 => 'not_found',
      409 => 'conflict',
      413 => 'too_large',
      429 => 'rate_limited',
      502 => 'attachment_unavailable',
      _ => 'internal',
    };
    var message = '请求失败（HTTP $statusCode）';
    int? version;
    Map<String, dynamic>? detail;
    try {
      final decoded = body.isEmpty ? null : jsonDecode(body);
      if (decoded is Map) {
        final err = asMap(decoded['error']);
        if (err.isNotEmpty) {
          code = asString(err['code'], code);
          message = asString(err['message'], message);
        }
        version = asIntOrNull(err['version']) ?? asIntOrNull(decoded['version']);
        final d = asMap(decoded['detail']);
        if (d.isNotEmpty) detail = d;
      }
    } catch (_) {
      // body 非 JSON：忽略，用兜底。
    }
    int? retryAfter;
    final ra = headers?['retry-after'];
    if (ra != null) retryAfter = int.tryParse(ra);
    return ApiException(
      code: code,
      message: message,
      statusCode: statusCode,
      version: version,
      retryAfterSeconds: retryAfter,
      detail: detail,
    );
  }

  @override
  String toString() => 'ApiException($code, $statusCode): $message';
}

/// 网络层错误（连不上 / 超时 / DNS / TLS）——与 ApiException 区分，
/// UI 据此显示「无法连接中枢」而非业务错误文案。
/// 对反馈管理写操作而言，网络错误意味着「结果待确认」（请求可能已到达）。
class ApiNetworkException implements Exception {
  const ApiNetworkException(this.cause);

  final Object cause;

  @override
  String toString() => 'ApiNetworkException: $cause';
}
