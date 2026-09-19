import 'dart:convert';

import '../models/json.dart';

/// 中枢错误信封：`{ "error": { "code": "…", "message": "…" } }`。
class ApiException implements Exception {
  const ApiException({
    required this.code,
    required this.message,
    this.statusCode,
    this.version,
    this.retryAfterSeconds,
  });

  final String code;
  final String message;
  final int? statusCode;

  /// `version_conflict` 时响应携带的当前版本号（位置契约未明确，宽容解析：
  /// error 块内与顶层都尝试）。
  final int? version;

  /// `rate_limited` 时 Retry-After 秒数。
  final int? retryAfterSeconds;

  bool get isUnauthorized => code == 'unauthorized' || statusCode == 401;
  bool get isVersionConflict => code == 'version_conflict';
  bool get isRateLimited => code == 'rate_limited';
  bool get isAttachmentUnavailable => code == 'attachment_unavailable';
  bool get isNotFound => code == 'not_found' || statusCode == 404;

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
    try {
      final decoded = body.isEmpty ? null : jsonDecode(body);
      if (decoded is Map) {
        final err = asMap(decoded['error']);
        if (err.isNotEmpty) {
          code = asString(err['code'], code);
          message = asString(err['message'], message);
        }
        version = asIntOrNull(err['version']) ?? asIntOrNull(decoded['version']);
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
    );
  }

  @override
  String toString() => 'ApiException($code, $statusCode): $message';
}

/// 网络层错误（连不上 / 超时 / DNS / TLS）——与 ApiException 区分，
/// UI 据此显示「无法连接中枢」而非业务错误文案。
class ApiNetworkException implements Exception {
  const ApiNetworkException(this.cause);

  final Object cause;

  @override
  String toString() => 'ApiNetworkException: $cause';
}
