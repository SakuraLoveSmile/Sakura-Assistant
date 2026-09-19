import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:http/http.dart' as http;

import '../models/fault.dart';
import '../models/feedback.dart';
import '../models/json.dart';
import '../models/message.dart';
import '../models/metrics.dart';
import '../models/overview.dart';
import '../models/rules.dart';
import '../models/session.dart';
import '../models/settings.dart';
import '../models/source.dart';
import '../models/sync.dart';
import 'api_exception.dart';

/// 登录结果（`POST /api/v1/auth/login` / `/auth/refresh` 响应）。
class LoginResult {
  const LoginResult({
    required this.token,
    required this.refreshToken,
    required this.username,
    this.expiresAt,
    this.serverTime,
  });

  final String token;
  final String refreshToken;
  final String username;
  final DateTime? expiresAt;
  final DateTime? serverTime;

  factory LoginResult.fromJson(Map<String, dynamic> json) => LoginResult(
        token: asString(json['token']),
        refreshToken: asString(json['refreshToken']),
        username: asString(asMap(json['user'])['username']),
        expiresAt: asDateOrNull(json['expiresAt']),
        serverTime: asDateOrNull(json['serverTime']),
      );
}

class MessagePage {
  const MessagePage({required this.items, this.nextCursor});

  final List<Message> items;
  final String? nextCursor;
}

class FaultPage {
  const FaultPage({required this.items, this.nextCursor});

  final List<Fault> items;
  final String? nextCursor;
}

/// `GET /api/v1/messages/:id` 响应。
class MessageDetail {
  const MessageDetail({required this.message, this.fault});

  final Message message;
  final Fault? fault;
}

/// 创建来源 / 轮换密钥的 `install` 块（一次性展示）。
class InstallInfo {
  const InstallInfo({required this.env, required this.command, this.note});

  final Map<String, String> env;
  final String command;
  final String? note;

  factory InstallInfo.fromJson(Map<String, dynamic> json) => InstallInfo(
        env: asStringMap(json['env']),
        command: asString(json['command']),
        note: asStringOrNull(json['note']),
      );
}

/// `POST /api/v1/sources` 201 响应。
class CreatedSource {
  const CreatedSource({required this.source, required this.accessKey, this.install});

  final Source source;
  final String accessKey;
  final InstallInfo? install;
}

/// `POST /api/v1/sources/:id/rotate-key` 响应。
class RotatedKey {
  const RotatedKey({required this.accessKey, this.install});

  final String accessKey;
  final InstallInfo? install;
}

/// 附件字节（日志预览 / 图片加载用）。
class AttachmentBytes {
  const AttachmentBytes({required this.bytes, required this.contentType});

  final List<int> bytes;
  final String contentType;
}

/// `GET /api/v1/sources/{srcId}/feedback` 响应页（api-v1 §3.1）。
class FeedbackListResult {
  const FeedbackListResult({
    required this.items,
    this.nextCursor,
    this.counts = const {},
    this.sourceId,
    this.sourceName,
  });

  final List<FeedbackItem> items;
  final String? nextCursor;

  /// 同一 q/筛选下三区计数：`{inbox, archived, trash}`。
  final Map<String, int> counts;
  final String? sourceId;
  final String? sourceName;
}

/// 历史页 filter 取值（契约 `filter=all|unread|feedback|faults`）。
enum MessageFilter { all, unread, feedback, faults }

/// Assistant 中枢 REST API 抽象。widget 测试用 Fake 实现替换。
abstract class AssistantApi {
  /// 绑定/更新会话（登录、刷新后调用）；null = 未登录。
  void bindSession(Session? session);

  Session? get session;

  // ---- 认证组 ----
  Future<LoginResult> login({
    required String hubUrl,
    required String username,
    required String password,
    String? deviceLabel,
  });

  Future<LoginResult> refreshSession();

  Future<void> logout();

  // ---- 客户端读取组 ----
  Future<Overview> getOverview();

  Future<SyncPage> getSync({int since = 0, int limit = 200});

  Future<MessagePage> getMessages({
    String? cursor,
    int limit = 50,
    MessageFilter filter = MessageFilter.all,
    String? source,
  });

  Future<MessageDetail> getMessage(String id);

  /// 附件字节地址（Image/network 或下载用）。attachmentId 可能含 `/`
  /// （如 `logs/<logId>`）：逐段编码保留路径结构。
  Uri attachmentUri(String messageId, String attachmentId);

  /// 拉附件字节（带鉴权）。
  Future<AttachmentBytes> fetchAttachment(String messageId, String attachmentId);

  Future<Message> markMessageRead(String id);

  Future<Message> markMessageUnread(String id);

  Future<int> markAllRead({DateTime? before});

  Future<FaultPage> getFaults({String state = 'open', String? cursor, int limit = 50});

  Future<Fault> markFaultRead(String id);

  Future<Fault> muteFault(String id);

  Future<Fault> unmuteFault(String id);

  Future<List<MetricPoint>> getMetricSeries({
    required String source,
    required String metric,
    required DateTime from,
    required DateTime to,
    String step = 'raw',
    String? label,
  });

  // ---- 管理组 ----
  Future<List<Source>> getSources();

  Future<CreatedSource> createSource({
    required String name,
    required String kind,
    String? attachmentBaseUrl,
  });

  Future<Source> updateSource(
    String id, {
    String? name,
    bool? enabled,
    String? attachmentBaseUrl,
    bool clearAttachmentBaseUrl = false,
  });

  Future<RotatedKey> rotateSourceKey(String id);

  Future<void> deleteSource(String id);

  Future<Rules> getRules();

  Future<Rules> putRules({
    required int expectedVersion,
    int? heartbeatSeconds,
    required List<AlertRule> rules,
  });

  Future<Settings> getSettings();

  Future<Settings> patchSettings({
    required int expectedVersion,
    DndSettings? dnd,
    int? reportIntervalSeconds,
  });

  // ---- Feedback 管理代理组（api-v1 §3.1，客户端令牌 + 按来源命名空间隔离） ----

  /// 管理列表：`view` ∈ inbox|archived|trash|all；`q` 匹配 title/text/id。
  Future<FeedbackListResult> getFeedbackList({
    required String sourceId,
    String view = 'inbox',
    String? cursor,
    int limit = 50,
    String? q,
  });

  /// 反馈详情（只读凭证回连透传 + `capabilities.manage` 由中枢重算）。
  Future<FeedbackDetail> getFeedbackDetail({
    required String sourceId,
    required String feedbackId,
  });

  /// 管理操作。`requestId` 由调用方按次生成（`generateFeedbackRequestId`）；
  /// 同一操作的超时重发必须复用同一值（服务端幂等去重）。
  Future<FeedbackActionResult> postFeedbackAction({
    required String sourceId,
    required String feedbackId,
    required String requestId,
    required String action,
    int? expectedLifecycleVersion,
    int? expectedRevision,
  });

  /// 反馈附件字节地址：`attachmentId` ∈ `screenshot` | `logs/<logId>`
  /// （路径通配，逐段编码保留结构）。
  Uri feedbackAttachmentUri(
      String sourceId, String feedbackId, String attachmentId);

  /// 拉反馈附件字节（带鉴权）。
  Future<AttachmentBytes> fetchFeedbackAttachment(
      String sourceId, String feedbackId, String attachmentId);
}

/// HTTP 实现：Bearer 令牌、401 → refresh → 重试一次 → 仍失败则登出回调。
class HttpAssistantApi implements AssistantApi {
  HttpAssistantApi({
    http.Client? client,
    this.onSessionRefreshed,
    this.onSessionExpired,
    this.timeout = const Duration(seconds: 15),
  }) : _client = client ?? http.Client();

  final http.Client _client;
  final Duration timeout;

  /// 刷新成功后回调（持久化新 token + refreshToken）。
  final void Function(Session session)? onSessionRefreshed;

  /// refresh 也失败 → 回调让会话层登出。
  final void Function()? onSessionExpired;

  Session? _session;
  Future<LoginResult>? _refreshing;

  @override
  Session? get session => _session;

  @override
  void bindSession(Session? session) => _session = session;

  Uri _uri(String path, [Map<String, String?>? query]) {
    final base = _session?.hubUrl ?? '';
    final normalized =
        base.endsWith('/') ? base.substring(0, base.length - 1) : base;
    final uri = Uri.parse('$normalized$path');
    if (query == null) return uri;
    final q = <String, String>{}
      ..addAll(uri.queryParameters)
      ..addEntries(query.entries
          .where((e) => e.value != null)
          .map((e) => MapEntry(e.key, e.value!)));
    return uri.replace(queryParameters: q);
  }

  Future<Map<String, dynamic>> _json(
    String method,
    String path, {
    Map<String, String?>? query,
    Object? body,
    bool retryOnUnauthorized = true,
    Duration? requestTimeout,
  }) async {
    final res = await _send(method, path,
        query: query, body: body, requestTimeout: requestTimeout);
    if (res.statusCode == 401 && retryOnUnauthorized) {
      final refreshed = await _tryRefresh();
      if (refreshed) {
        return _json(method, path,
            query: query,
            body: body,
            retryOnUnauthorized: false,
            requestTimeout: requestTimeout);
      }
      onSessionExpired?.call();
      throw ApiException.fromBody(res.statusCode, res.body, headers: res.headers);
    }
    if (res.statusCode >= 200 && res.statusCode < 300) {
      if (res.body.isEmpty) return <String, dynamic>{};
      final decoded = jsonDecode(res.body);
      if (decoded is Map<String, dynamic>) return decoded;
      if (decoded is Map) return decoded.map((k, v) => MapEntry(k.toString(), v));
      // 顶层数组等异常情况兜底。
      return <String, dynamic>{'items': decoded};
    }
    throw ApiException.fromBody(res.statusCode, res.body, headers: res.headers);
  }

  Future<http.Response> _send(
    String method,
    String path, {
    Map<String, String?>? query,
    Object? body,
    Map<String, String>? extraHeaders,
    Duration? requestTimeout,
  }) async {
    final uri = _uri(path, query);
    final headers = <String, String>{
      'Accept': 'application/json',
      if (body != null) 'Content-Type': 'application/json',
      if (_session?.token != null) 'Authorization': 'Bearer ${_session!.token}',
      ...?extraHeaders,
    };
    final request = http.Request(method, uri)
      ..headers.addAll(headers);
    if (body != null) request.body = jsonEncode(body);
    try {
      final streamed =
          await _client.send(request).timeout(requestTimeout ?? timeout);
      return await http.Response.fromStream(streamed);
    } on TimeoutException catch (e) {
      throw ApiNetworkException(e);
    } on SocketException catch (e) {
      throw ApiNetworkException(e);
    } on http.ClientException catch (e) {
      throw ApiNetworkException(e);
    }
  }

  Future<bool> _tryRefresh() async {
    if ((_session?.refreshToken ?? '').isEmpty) return false;
    _refreshing ??= _doRefresh();
    try {
      final result = await _refreshing;
      final s = _session;
      if (result == null || s == null) return false;
      final next = s.copyWith(
        token: result.token,
        refreshToken: result.refreshToken,
        expiresAt: result.expiresAt,
      );
      _session = next;
      onSessionRefreshed?.call(next);
      return true;
    } catch (_) {
      return false;
    } finally {
      _refreshing = null;
    }
  }

  Future<LoginResult> _doRefresh() async {
    final res = await _send('POST', '/api/v1/auth/refresh',
        body: {'refreshToken': _session!.refreshToken});
    if (res.statusCode == 200) {
      return LoginResult.fromJson(asMap(jsonDecode(res.body)));
    }
    throw ApiException.fromBody(res.statusCode, res.body, headers: res.headers);
  }

  // ---------------- 认证组 ----------------

  @override
  Future<LoginResult> login({
    required String hubUrl,
    required String username,
    required String password,
    String? deviceLabel,
  }) async {
    // 登录请求直接打到给定 hubUrl，不依赖已绑定会话。
    final normalized =
        hubUrl.endsWith('/') ? hubUrl.substring(0, hubUrl.length - 1) : hubUrl;
    final uri = Uri.parse('$normalized/api/v1/auth/login');
    http.Response res;
    try {
      res = await _client
          .post(
            uri,
            headers: {
              'Accept': 'application/json',
              'Content-Type': 'application/json',
            },
            body: jsonEncode({
              'username': username,
              'password': password,
              if (deviceLabel != null && deviceLabel.isNotEmpty)
                'deviceLabel': deviceLabel,
            }),
          )
          .timeout(timeout);
    } on TimeoutException catch (e) {
      throw ApiNetworkException(e);
    } on SocketException catch (e) {
      throw ApiNetworkException(e);
    } on http.ClientException catch (e) {
      throw ApiNetworkException(e);
    }
    if (res.statusCode == 200) {
      return LoginResult.fromJson(asMap(jsonDecode(res.body)));
    }
    throw ApiException.fromBody(res.statusCode, res.body, headers: res.headers);
  }

  @override
  Future<LoginResult> refreshSession() async {
    final ok = await _tryRefresh();
    if (!ok || _session == null) {
      throw const ApiException(code: 'unauthorized', message: '刷新令牌失败');
    }
    final s = _session!;
    return LoginResult(
      token: s.token,
      refreshToken: s.refreshToken,
      username: s.username,
      expiresAt: s.expiresAt,
    );
  }

  @override
  Future<void> logout() async {
    try {
      await _send('POST', '/api/v1/auth/logout', body: const {});
    } catch (_) {
      // 登出失败不阻塞本地清理。
    }
  }

  // ---------------- 客户端读取组 ----------------

  @override
  Future<Overview> getOverview() async =>
      Overview.fromJson(await _json('GET', '/api/v1/overview'));

  @override
  Future<SyncPage> getSync({int since = 0, int limit = 200}) async =>
      SyncPage.fromJson(await _json('GET', '/api/v1/sync',
          query: {'since': '$since', 'limit': '$limit'}));

  @override
  Future<MessagePage> getMessages({
    String? cursor,
    int limit = 50,
    MessageFilter filter = MessageFilter.all,
    String? source,
  }) async {
    final json = await _json('GET', '/api/v1/messages', query: {
      'cursor': ?cursor,
      'limit': '$limit',
      'filter': filter.name,
      if (source != null && source.isNotEmpty) 'source': source,
    });
    return MessagePage(
      items: asMapList(json['items']).map(Message.fromJson).toList(),
      nextCursor: asStringOrNull(json['nextCursor']),
    );
  }

  @override
  Future<MessageDetail> getMessage(String id) async {
    final json = await _json('GET', '/api/v1/messages/${Uri.encodeComponent(id)}');
    return MessageDetail(
      message: Message.fromJson(asMap(json['message'])),
      fault: json['fault'] == null ? null : Fault.fromJson(asMap(json['fault'])),
    );
  }

  @override
  Uri attachmentUri(String messageId, String attachmentId) {
    // attachmentId 可能含 `/`（logs/<logId>）：逐段编码保留路径结构。
    final encodedAtt = attachmentId.split('/').map(Uri.encodeComponent).join('/');
    return _uri('/api/v1/messages/${Uri.encodeComponent(messageId)}/attachments/$encodedAtt');
  }

  @override
  Future<AttachmentBytes> fetchAttachment(String messageId, String attachmentId) async {
    final uri = attachmentUri(messageId, attachmentId);
    http.Response res;
    try {
      res = await _client
          .get(uri, headers: {
            if (_session?.token != null) 'Authorization': 'Bearer ${_session!.token}',
          })
          .timeout(timeout);
    } on TimeoutException catch (e) {
      throw ApiNetworkException(e);
    } on SocketException catch (e) {
      throw ApiNetworkException(e);
    } on http.ClientException catch (e) {
      throw ApiNetworkException(e);
    }
    if (res.statusCode == 200) {
      return AttachmentBytes(
        bytes: res.bodyBytes,
        contentType: res.headers['content-type'] ?? 'application/octet-stream',
      );
    }
    throw ApiException.fromBody(res.statusCode, res.body, headers: res.headers);
  }

  @override
  Future<Message> markMessageRead(String id) async => Message.fromJson(
      asMap((await _json('POST', '/api/v1/messages/${Uri.encodeComponent(id)}/read',
          body: const {}))['message']));

  @override
  Future<Message> markMessageUnread(String id) async => Message.fromJson(asMap(
      (await _json('POST', '/api/v1/messages/${Uri.encodeComponent(id)}/unread',
          body: const {}))['message']));

  @override
  Future<int> markAllRead({DateTime? before}) async {
    final json = await _json('POST', '/api/v1/messages/read-all', body: {
      'before': before?.toUtc().toIso8601String(),
    });
    return asInt(json['updated']);
  }

  @override
  Future<FaultPage> getFaults({String state = 'open', String? cursor, int limit = 50}) async {
    final json = await _json('GET', '/api/v1/faults', query: {
      'state': state,
      'cursor': ?cursor,
      'limit': '$limit',
    });
    return FaultPage(
      items: asMapList(json['items']).map(Fault.fromJson).toList(),
      nextCursor: asStringOrNull(json['nextCursor']),
    );
  }

  @override
  Future<Fault> markFaultRead(String id) async => Fault.fromJson(asMap(
      (await _json('POST', '/api/v1/faults/${Uri.encodeComponent(id)}/read',
          body: const {}))['fault']));

  @override
  Future<Fault> muteFault(String id) async => Fault.fromJson(asMap(
      (await _json('POST', '/api/v1/faults/${Uri.encodeComponent(id)}/mute',
          body: const {}))['fault']));

  @override
  Future<Fault> unmuteFault(String id) async => Fault.fromJson(asMap(
      (await _json('POST', '/api/v1/faults/${Uri.encodeComponent(id)}/unmute',
          body: const {}))['fault']));

  @override
  Future<List<MetricPoint>> getMetricSeries({
    required String source,
    required String metric,
    required DateTime from,
    required DateTime to,
    String step = 'raw',
    String? label,
  }) async {
    final json = await _json('GET', '/api/v1/metrics/series', query: {
      'source': source,
      'metric': metric,
      'from': from.toUtc().toIso8601String(),
      'to': to.toUtc().toIso8601String(),
      'step': step,
      if (label != null && label.isNotEmpty) 'label': label,
    });
    return asMapList(json['series']).map(MetricPoint.fromJson).toList();
  }

  // ---------------- 管理组 ----------------

  @override
  Future<List<Source>> getSources() async {
    final json = await _json('GET', '/api/v1/sources');
    return asMapList(json['sources']).map(Source.fromJson).toList();
  }

  @override
  Future<CreatedSource> createSource({
    required String name,
    required String kind,
    String? attachmentBaseUrl,
  }) async {
    final json = await _json('POST', '/api/v1/sources', body: {
      'name': name,
      'kind': kind,
      if (attachmentBaseUrl != null && attachmentBaseUrl.isNotEmpty)
        'attachmentBaseUrl': attachmentBaseUrl,
    });
    return CreatedSource(
      source: Source.fromJson(asMap(json['source'])),
      accessKey: asString(json['accessKey']),
      install: json['install'] == null ? null : InstallInfo.fromJson(asMap(json['install'])),
    );
  }

  @override
  Future<Source> updateSource(
    String id, {
    String? name,
    bool? enabled,
    String? attachmentBaseUrl,
    bool clearAttachmentBaseUrl = false,
  }) async {
    final json = await _json('PATCH', '/api/v1/sources/${Uri.encodeComponent(id)}', body: {
      'name': ?name,
      'enabled': ?enabled,
      'attachmentBaseUrl': ?attachmentBaseUrl,
      if (clearAttachmentBaseUrl) 'attachmentBaseUrl': null,
    });
    return Source.fromJson(asMap(json['source']));
  }

  @override
  Future<RotatedKey> rotateSourceKey(String id) async {
    final json =
        await _json('POST', '/api/v1/sources/${Uri.encodeComponent(id)}/rotate-key', body: const {});
    return RotatedKey(
      accessKey: asString(json['accessKey']),
      install: json['install'] == null ? null : InstallInfo.fromJson(asMap(json['install'])),
    );
  }

  @override
  Future<void> deleteSource(String id) async =>
      _json('DELETE', '/api/v1/sources/${Uri.encodeComponent(id)}');

  @override
  Future<Rules> getRules() async => Rules.fromJson(await _json('GET', '/api/v1/rules'));

  @override
  Future<Rules> putRules({
    required int expectedVersion,
    int? heartbeatSeconds,
    required List<AlertRule> rules,
  }) async {
    final json = await _json('PUT', '/api/v1/rules', body: {
      'expectedVersion': expectedVersion,
      'heartbeatSeconds': ?heartbeatSeconds,
      'rules': rules.map((r) => r.toJson()).toList(),
    });
    return Rules.fromJson(json);
  }

  @override
  Future<Settings> getSettings() async =>
      Settings.fromJson(await _json('GET', '/api/v1/settings'));

  @override
  Future<Settings> patchSettings({
    required int expectedVersion,
    DndSettings? dnd,
    int? reportIntervalSeconds,
  }) async {
    final json = await _json('PATCH', '/api/v1/settings', body: {
      'expectedVersion': expectedVersion,
      'dnd': ?dnd?.toJson(),
      'reportIntervalSeconds': ?reportIntervalSeconds,
    });
    return Settings.fromJson(json);
  }

  // ---------------- Feedback 管理代理组（api-v1 §3.1） ----------------

  @override
  Future<FeedbackListResult> getFeedbackList({
    required String sourceId,
    String view = 'inbox',
    String? cursor,
    int limit = 50,
    String? q,
  }) async {
    final json = await _json(
      'GET',
      '/api/v1/sources/${Uri.encodeComponent(sourceId)}/feedback',
      query: {
        'view': view,
        'cursor': ?cursor,
        'limit': '$limit',
        if (q != null && q.isNotEmpty) 'q': q,
      },
    );
    return FeedbackListResult(
      items: asMapList(json['items']).map(FeedbackItem.fromJson).toList(),
      nextCursor: asStringOrNull(json['nextCursor']),
      counts:
          asMap(json['counts']).map((k, v) => MapEntry(k.toString(), asInt(v))),
      sourceId: asStringOrNull(json['sourceId']),
      sourceName: asStringOrNull(json['sourceName']),
    );
  }

  @override
  Future<FeedbackDetail> getFeedbackDetail({
    required String sourceId,
    required String feedbackId,
  }) async {
    final json = await _json(
      'GET',
      '/api/v1/sources/${Uri.encodeComponent(sourceId)}'
      '/feedback/${Uri.encodeComponent(feedbackId)}',
    );
    return FeedbackDetail.fromJson(json);
  }

  @override
  Future<FeedbackActionResult> postFeedbackAction({
    required String sourceId,
    required String feedbackId,
    required String requestId,
    required String action,
    int? expectedLifecycleVersion,
    int? expectedRevision,
  }) async {
    final json = await _json(
      'POST',
      '/api/v1/sources/${Uri.encodeComponent(sourceId)}'
      '/feedback/${Uri.encodeComponent(feedbackId)}/action',
      body: {
        'requestId': requestId,
        'action': action,
        'expectedLifecycleVersion': ?expectedLifecycleVersion,
        'expectedRevision': ?expectedRevision,
      },
      // 中枢回连上游超时 30s：客户端留余量；超时后按「结果待确认」处理。
      requestTimeout: const Duration(seconds: 35),
    );
    return FeedbackActionResult.fromJson(json);
  }

  @override
  Uri feedbackAttachmentUri(
      String sourceId, String feedbackId, String attachmentId) {
    // attachmentId 可能含 `/`（logs/<logId>）：逐段编码保留路径结构。
    final encodedAtt =
        attachmentId.split('/').map(Uri.encodeComponent).join('/');
    return _uri('/api/v1/sources/${Uri.encodeComponent(sourceId)}'
        '/feedback/${Uri.encodeComponent(feedbackId)}/attachments/$encodedAtt');
  }

  @override
  Future<AttachmentBytes> fetchFeedbackAttachment(
      String sourceId, String feedbackId, String attachmentId) async {
    final uri = feedbackAttachmentUri(sourceId, feedbackId, attachmentId);
    http.Response res;
    try {
      res = await _client
          .get(uri, headers: {
            if (_session?.token != null)
              'Authorization': 'Bearer ${_session!.token}',
          })
          .timeout(timeout);
    } on TimeoutException catch (e) {
      throw ApiNetworkException(e);
    } on SocketException catch (e) {
      throw ApiNetworkException(e);
    } on http.ClientException catch (e) {
      throw ApiNetworkException(e);
    }
    if (res.statusCode == 200) {
      return AttachmentBytes(
        bytes: res.bodyBytes,
        contentType: res.headers['content-type'] ?? 'application/octet-stream',
      );
    }
    throw ApiException.fromBody(res.statusCode, res.body, headers: res.headers);
  }
}
