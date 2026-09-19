import 'json.dart';

/// 附件描述符（字节经 `/api/v1/messages/:id/attachments/:attId` 按需拉取）。
class Attachment {
  const Attachment({
    required this.id,
    required this.kind,
    required this.filename,
    required this.mime,
    required this.byteSize,
    this.sha256,
  });

  final String id;
  final String kind; // screenshot | log | ...
  final String filename;
  final String mime;
  final int byteSize;
  final String? sha256;

  bool get isImage => mime.startsWith('image/');
  bool get isLog => kind == 'log' || mime.startsWith('text/');

  factory Attachment.fromJson(Map<String, dynamic> json) => Attachment(
        id: asString(json['id']),
        kind: asString(json['kind']),
        filename: asString(json['filename']),
        mime: asString(json['mime'], 'application/octet-stream'),
        byteSize: asInt(json['byteSize']),
        sha256: asStringOrNull(json['sha256']),
      );

  Map<String, dynamic> toJson() => {
        'id': id,
        'kind': kind,
        'filename': filename,
        'mime': mime,
        'byteSize': byteSize,
        if (sha256 != null) 'sha256': sha256,
      };
}

/// 消息对象（api-v1 §3 Message）。SSE 推送时额外携带 [notify] 提示块。
class Message {
  const Message({
    required this.id,
    required this.changeSeq,
    required this.sourceId,
    required this.sourceName,
    required this.kind,
    required this.severity,
    required this.title,
    this.body,
    required this.occurredAt,
    required this.receivedAt,
    this.readAt,
    this.faultId,
    this.incident,
    this.ref,
    this.attachments = const [],
    this.notify,
  });

  final String id;
  final int changeSeq;
  final String sourceId;
  final String sourceName;
  final String kind; // feedback_created | fault_open | heartbeat_lost | ...
  final String severity; // info | warning | critical
  final String title;
  final String? body;
  final DateTime occurredAt;
  final DateTime receivedAt;
  final DateTime? readAt;
  final String? faultId;
  final int? incident;
  final Map<String, dynamic>? ref;
  final List<Attachment> attachments;
  final NotifyHint? notify;

  bool get isRead => readAt != null;
  bool get belongsToFault => faultId != null;

  /// 引用的 feedbackId（`ref.feedbackId`），没有则为 null。
  String? get feedbackId => asStringOrNull(ref?['feedbackId']);

  factory Message.fromJson(Map<String, dynamic> json) => Message(
        id: asString(json['id']),
        changeSeq: asInt(json['changeSeq']),
        sourceId: asString(json['sourceId']),
        sourceName: asString(json['sourceName']),
        kind: asString(json['kind'], 'custom'),
        severity: asString(json['severity'], 'info'),
        title: asString(json['title']),
        body: asStringOrNull(json['body']),
        occurredAt: asDate(json['occurredAt']),
        receivedAt: asDate(json['receivedAt']),
        readAt: asDateOrNull(json['readAt']),
        faultId: asStringOrNull(json['faultId']),
        incident: asIntOrNull(json['incident']),
        ref: json['ref'] == null ? null : asMap(json['ref']),
        attachments:
            asMapList(json['attachments']).map(Attachment.fromJson).toList(),
        notify:
            json['notify'] == null ? null : NotifyHint.fromJson(asMap(json['notify'])),
      );

  Map<String, dynamic> toJson() => {
        'id': id,
        'changeSeq': changeSeq,
        'sourceId': sourceId,
        'sourceName': sourceName,
        'kind': kind,
        'severity': severity,
        'title': title,
        'body': body,
        'occurredAt': occurredAt.toUtc().toIso8601String(),
        'receivedAt': receivedAt.toUtc().toIso8601String(),
        'readAt': readAt?.toUtc().toIso8601String(),
        'faultId': faultId,
        'incident': incident,
        'ref': ref,
        'attachments': attachments.map((a) => a.toJson()).toList(),
      };

  Message copyWith({DateTime? readAt, bool clearReadAt = false}) => Message(
        id: id,
        changeSeq: changeSeq,
        sourceId: sourceId,
        sourceName: sourceName,
        kind: kind,
        severity: severity,
        title: title,
        body: body,
        occurredAt: occurredAt,
        receivedAt: receivedAt,
        readAt: clearReadAt ? null : (readAt ?? this.readAt),
        faultId: faultId,
        incident: incident,
        ref: ref,
        attachments: attachments,
        notify: notify,
      );
}

/// SSE `message` 事件附带的通知提示块（REST 响应不含）。
class NotifyHint {
  const NotifyHint({required this.kind, this.faultId, this.muted = false});

  final String kind; // new_message | incident_open | incident_update | incident_resolved
  final String? faultId;
  final bool muted;

  factory NotifyHint.fromJson(Map<String, dynamic> json) => NotifyHint(
        kind: asString(json['kind']),
        faultId: asStringOrNull(json['faultId']),
        muted: asBool(json['muted']),
      );
}
