import 'dart:math';

import 'json.dart';

/// 反馈管理视图（api-v1 §3.1 `view` 参数；`all` = 收件箱+已归档，不含回收站）。
enum FeedbackView {
  inbox('inbox'),
  archived('archived'),
  trash('trash'),
  all('all');

  const FeedbackView(this.wire);

  /// 线上取值。
  final String wire;

  static FeedbackView fromWire(String? v) {
    for (final view in values) {
      if (view.wire == v) return view;
    }
    return inbox;
  }
}

/// 管理动作（feedback-integration §4.2 固定七项，与 `allowedActions` 口径一致）。
enum FeedbackAction {
  archive('archive'),
  unarchive('unarchive'),
  trash('trash'),
  restore('restore'),
  resumeProcessing('resume_processing'),
  retry('retry'),
  recheck('recheck');

  const FeedbackAction(this.wire);

  /// 线上取值。
  final String wire;

  /// 生命周期动作（applyLifecycleInTx）：请求体必填 `expectedLifecycleVersion`。
  bool get isLifecycle => switch (this) {
        archive || unarchive || trash || restore || resumeProcessing => true,
        retry || recheck => false,
      };

  /// retry / recheck（worker 路径）：请求体必填 `expectedRevision`。
  bool get needsRevision => this == retry || this == recheck;

  static FeedbackAction? fromWire(String? v) {
    for (final action in values) {
      if (action.wire == v) return action;
    }
    return null;
  }
}

/// 反馈列表条目（feedback-integration §4.1 列表元素；旧版服务端缺管理字段时容忍）。
class FeedbackItem {
  const FeedbackItem({
    required this.id,
    this.appId = '',
    this.appName,
    this.status = '',
    this.issueStatus,
    this.mgmtState,
    this.lifecycleVersion,
    this.title,
    this.textPreview,
    this.createdAt,
    this.updatedAt,
    this.errorSummary,
    this.hasScreenshot = false,
    this.logCount = 0,
    this.collectionState,
    this.resumePaused = false,
    this.allowedActions = const [],
  });

  final String id;
  final String appId;
  final String? appName;
  final String status;
  final String? issueStatus;

  /// 收件箱生命周期：inbox | archived | trash（旧版服务端缺席）。
  final String? mgmtState;

  /// 生命周期乐观锁版本（缺席 = 旧版无管理面）。
  final int? lifecycleVersion;
  final String? title;
  final String? textPreview;
  final DateTime? createdAt;
  final DateTime? updatedAt;

  /// 错误摘要（非空 = 处理出错过，列表上显示徽标）。
  final String? errorSummary;
  final bool hasScreenshot;
  final int logCount;
  final String? collectionState;
  final bool resumePaused;

  /// 当前允许的管理动作（生命周期动作子集）。
  final List<String> allowedActions;

  /// 展示标题：title 为空时回退预览文本。
  String get displayTitle {
    final t = title;
    if (t != null && t.isNotEmpty) return t;
    final p = textPreview;
    if (p != null && p.isNotEmpty) return p;
    return '（无标题）';
  }

  factory FeedbackItem.fromJson(Map<String, dynamic> json) => FeedbackItem(
        id: asString(json['id']),
        appId: asString(json['appId']),
        appName: asStringOrNull(json['appName']),
        status: asString(json['status']),
        issueStatus: asStringOrNull(json['issueStatus']),
        mgmtState: asStringOrNull(json['mgmtState']),
        lifecycleVersion: asIntOrNull(json['lifecycleVersion']),
        title: asStringOrNull(json['title']),
        textPreview: asStringOrNull(json['textPreview']),
        createdAt: asDateOrNull(json['createdAt']),
        updatedAt: asDateOrNull(json['updatedAt']),
        errorSummary: asStringOrNull(json['errorSummary']),
        hasScreenshot: asBool(json['hasScreenshot']),
        logCount: asInt(json['logCount']),
        collectionState: asStringOrNull(json['collectionState']),
        resumePaused: asBool(json['resumePaused']),
        allowedActions: asList(json['allowedActions'])
            .map((e) => asString(e))
            .where((e) => e.isNotEmpty)
            .toList(),
      );
}

/// 反馈日志附件描述（只读详情携带；字节经附件接口按需拉取）。
class FeedbackLogEntry {
  const FeedbackLogEntry({
    required this.id,
    this.filename = '',
    this.byteSize = 0,
    this.sha256,
    this.source,
  });

  final String id;
  final String filename;
  final int byteSize;
  final String? sha256;
  final String? source;

  factory FeedbackLogEntry.fromJson(Map<String, dynamic> json) =>
      FeedbackLogEntry(
        id: asString(json['id']),
        filename: asString(json['filename']),
        byteSize: asInt(json['byteSize']),
        sha256: asStringOrNull(json['sha256']),
        source: asStringOrNull(json['source']),
      );
}

/// 反馈详情（api-v1 §3.1 `GET /sources/{src}/feedback/{fb}` 透传 +
/// `sourceId`/`sourceName`）。v1.1 管理字段在旧版服务端缺席——全部可空容忍。
class FeedbackDetail {
  const FeedbackDetail({
    required this.id,
    this.appId = '',
    this.appName,
    this.status = '',
    this.title,
    this.text,
    this.createdAt,
    this.updatedAt,
    this.errorSummary,
    this.hasScreenshot = false,
    this.logs = const [],
    this.mgmtState,
    this.lifecycleVersion,
    this.revision,
    this.issueStatus,
    this.collectionState,
    this.resumePaused = false,
    this.archiveStage,
    this.kaneoTaskUrl,
    this.archivedAt,
    this.trashedAt,
    this.allowedActions = const [],
    this.capabilities = const {},
    this.sourceId,
    this.sourceName,
  });

  final String id;
  final String appId;
  final String? appName;
  final String status;
  final String? title;
  final String? text;
  final DateTime? createdAt;
  final DateTime? updatedAt;
  final String? errorSummary;
  final bool hasScreenshot;
  final List<FeedbackLogEntry> logs;

  // ---- v1.1 管理字段（旧版服务端缺席） ----
  final String? mgmtState;
  final int? lifecycleVersion;
  final int? revision;
  final String? issueStatus;
  final String? collectionState;
  final bool resumePaused;
  final String? archiveStage;
  final String? kaneoTaskUrl;
  final DateTime? archivedAt;
  final DateTime? trashedAt;
  final List<String> allowedActions;
  final Map<String, dynamic> capabilities;

  final String? sourceId;
  final String? sourceName;

  /// 端到端可管理（中枢重算：上游 manage && 本来源已配置 mgmtKey）。
  bool get manage => asBool(capabilities['manage']);

  /// 是否带有任何 v1.1 管理字段：全无 = 旧版服务端（显示「需升级」）；
  /// 有字段但 manage=false = 管理凭证/管理面问题（显示「需配置」）。
  bool get hasMgmtFields =>
      mgmtState != null ||
      lifecycleVersion != null ||
      revision != null ||
      allowedActions.isNotEmpty ||
      capabilities.isNotEmpty;

  /// 当前允许且客户端认识的动作集（未知动作忽略）。
  List<FeedbackAction> get knownActions => allowedActions
      .map(FeedbackAction.fromWire)
      .whereType<FeedbackAction>()
      .toList();

  String get displayTitle {
    final t = title;
    if (t != null && t.isNotEmpty) return t;
    final p = text;
    if (p != null && p.isNotEmpty) {
      final firstLine = p.split('\n').first.trim();
      return firstLine.isEmpty ? '（无标题）' : firstLine;
    }
    return '（无标题）';
  }

  factory FeedbackDetail.fromJson(Map<String, dynamic> json) => FeedbackDetail(
        id: asString(json['id']),
        appId: asString(json['appId']),
        appName: asStringOrNull(json['appName']),
        status: asString(json['status']),
        title: asStringOrNull(json['title']),
        text: asStringOrNull(json['text']),
        createdAt: asDateOrNull(json['createdAt']),
        updatedAt: asDateOrNull(json['updatedAt']),
        errorSummary: asStringOrNull(json['errorSummary']),
        hasScreenshot: asBool(json['hasScreenshot']),
        logs: asMapList(json['logs']).map(FeedbackLogEntry.fromJson).toList(),
        mgmtState: asStringOrNull(json['mgmtState']),
        lifecycleVersion: asIntOrNull(json['lifecycleVersion']),
        revision: asIntOrNull(json['revision']),
        issueStatus: asStringOrNull(json['issueStatus']),
        collectionState: asStringOrNull(json['collectionState']),
        resumePaused: asBool(json['resumePaused']),
        archiveStage: asStringOrNull(json['archiveStage']),
        kaneoTaskUrl: asStringOrNull(json['kaneoTaskUrl']),
        archivedAt: asDateOrNull(json['archivedAt']),
        trashedAt: asDateOrNull(json['trashedAt']),
        allowedActions: asList(json['allowedActions'])
            .map((e) => asString(e))
            .where((e) => e.isNotEmpty)
            .toList(),
        capabilities: asMap(json['capabilities']),
        sourceId: asStringOrNull(json['sourceId']),
        sourceName: asStringOrNull(json['sourceName']),
      );
}

/// `POST …/feedback/{fb}/action` 成功响应（`detail` 为操作后的实时快照）。
class FeedbackActionResult {
  const FeedbackActionResult({
    required this.ok,
    required this.action,
    this.replayed = false,
    this.detail,
  });

  final bool ok;
  final String action;

  /// true = 同 requestId 的幂等回放（服务端未重复执行）。
  final bool replayed;
  final FeedbackDetail? detail;

  factory FeedbackActionResult.fromJson(Map<String, dynamic> json) =>
      FeedbackActionResult(
        ok: asBool(json['ok'], true),
        action: asString(json['action']),
        replayed: asBool(json['replayed']),
        detail: json['detail'] == null
            ? null
            : FeedbackDetail.fromJson(asMap(json['detail'])),
      );
}

// ---------------- requestId（ULID） ----------------

Random? _random;

Random _rng() {
  final r = _random;
  if (r != null) return r;
  try {
    return _random = Random.secure();
  } on UnsupportedError {
    // 极少数环境不支持 CSPRNG：退回时间种子（仅保证唯一性）。
    final seed = DateTime.now().microsecondsSinceEpoch ^
        DateTime.now().millisecondsSinceEpoch;
    return _random = Random(seed);
  }
}

const _ulidAlphabet = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';

/// 生成管理操作幂等键：`rop_` + 26 字符 ULID（48bit 毫秒时间 + 80bit 随机，
/// Crockford base32）。满足契约 `[A-Za-z0-9._:-]{1,128}`。
///
/// 每次操作生成唯一值；同一操作的重发必须复用同一 requestId（服务端按
/// `assist_mgmt_requests` 幂等去重，绝不重复执行）。
String generateFeedbackRequestId() {
  final rng = _rng();
  final buf = StringBuffer('rop_');
  // 时间 48bit → 10 字符（高位在前）。
  final time = DateTime.now().millisecondsSinceEpoch & 0xFFFFFFFFFFFF;
  for (var i = 9; i >= 0; i--) {
    buf.write(_ulidAlphabet[(time >> (5 * i)) & 0x1F]);
  }
  // 随机 80bit（10 字节）→ 16 字符，每 5bit 一字符。
  var buffer = 0;
  var bufferBits = 0;
  var emitted = 0;
  for (var i = 0; i < 10 && emitted < 16; i++) {
    buffer = (buffer << 8) | rng.nextInt(256);
    bufferBits += 8;
    while (bufferBits >= 5 && emitted < 16) {
      bufferBits -= 5;
      buf.write(_ulidAlphabet[(buffer >> bufferBits) & 0x1F]);
      emitted++;
    }
    buffer &= (1 << bufferBits) - 1;
  }
  return buf.toString();
}
