import 'json.dart';

/// 故障对象（api-v1 §3 Fault）。
///
/// 注意：`GET /api/v1/overview` 的 `openFaults` 元素是紧凑形式，
/// 用 `muted`/`read` 布尔代替 `mutedAt`/`readAt`，且没有 `state`/`summary`
/// 等字段 —— fromJson 两种形态都容忍。
class Fault {
  const Fault({
    required this.id,
    required this.changeSeq,
    required this.sourceId,
    required this.sourceName,
    required this.faultKey,
    required this.severity,
    required this.title,
    this.summary,
    this.state = 'open',
    this.incident = 1,
    required this.openedAt,
    required this.lastEventAt,
    this.resolvedAt,
    this.resolvedBy,
    this.eventCount = 0,
    this.readAt,
    this.mutedAt,
    this.mutedUntil,
  });

  final String id;
  final int changeSeq;
  final String sourceId;
  final String sourceName;
  final String faultKey;
  final String severity; // info | warning | critical
  final String title;
  final String? summary;
  final String state; // open | resolved
  final int incident;
  final DateTime openedAt;
  final DateTime lastEventAt;
  final DateTime? resolvedAt;
  final Object? resolvedBy;
  final int eventCount;
  final DateTime? readAt;
  final DateTime? mutedAt;
  final DateTime? mutedUntil;

  bool get isOpen => state == 'open';
  bool get isResolved => state == 'resolved';

  /// 静音挂在整个 Fault 上、跨轮次持续。
  bool get muted {
    if (mutedAt == null) return false;
    final until = mutedUntil;
    if (until != null && until.isBefore(DateTime.now())) return false;
    return true;
  }

  bool get isRead => readAt != null;

  factory Fault.fromJson(Map<String, dynamic> json) {
    // 紧凑形态（overview.openFaults）：muted/read 布尔。
    final mutedFlag = asBool(json['muted']);
    final readFlag = asBool(json['read']);
    final mutedAt = asDateOrNull(json['mutedAt']) ??
        (mutedFlag ? DateTime.fromMillisecondsSinceEpoch(0) : null);
    final readAt = asDateOrNull(json['readAt']) ??
        (readFlag ? DateTime.fromMillisecondsSinceEpoch(0) : null);
    return Fault(
      id: asString(json['id']),
      changeSeq: asInt(json['changeSeq']),
      sourceId: asString(json['sourceId']),
      sourceName: asString(json['sourceName']),
      faultKey: asString(json['faultKey']),
      severity: asString(json['severity'], 'warning'),
      title: asString(json['title']),
      summary: asStringOrNull(json['summary']),
      state: asString(json['state'], 'open'),
      incident: asInt(json['incident'], 1),
      openedAt: asDate(json['openedAt']),
      lastEventAt: asDate(json['lastEventAt']),
      resolvedAt: asDateOrNull(json['resolvedAt']),
      resolvedBy: json['resolvedBy'],
      eventCount: asInt(json['eventCount']),
      readAt: readAt,
      mutedAt: mutedAt,
      mutedUntil: asDateOrNull(json['mutedUntil']),
    );
  }

  Map<String, dynamic> toJson() => {
        'id': id,
        'changeSeq': changeSeq,
        'sourceId': sourceId,
        'sourceName': sourceName,
        'faultKey': faultKey,
        'severity': severity,
        'title': title,
        'summary': summary,
        'state': state,
        'incident': incident,
        'openedAt': openedAt.toUtc().toIso8601String(),
        'lastEventAt': lastEventAt.toUtc().toIso8601String(),
        'resolvedAt': resolvedAt?.toUtc().toIso8601String(),
        'resolvedBy': resolvedBy,
        'eventCount': eventCount,
        'readAt': readAt?.toUtc().toIso8601String(),
        'mutedAt': mutedAt?.toUtc().toIso8601String(),
        'mutedUntil': mutedUntil?.toUtc().toIso8601String(),
      };

  Fault copyWith({
    String? state,
    DateTime? readAt,
    DateTime? mutedAt,
    bool clearReadAt = false,
    bool clearMutedAt = false,
  }) =>
      Fault(
        id: id,
        changeSeq: changeSeq,
        sourceId: sourceId,
        sourceName: sourceName,
        faultKey: faultKey,
        severity: severity,
        title: title,
        summary: summary,
        state: state ?? this.state,
        incident: incident,
        openedAt: openedAt,
        lastEventAt: lastEventAt,
        resolvedAt: resolvedAt,
        resolvedBy: resolvedBy,
        eventCount: eventCount,
        readAt: clearReadAt ? null : (readAt ?? this.readAt),
        mutedAt: clearMutedAt ? null : (mutedAt ?? this.mutedAt),
        mutedUntil: mutedUntil,
      );
}
