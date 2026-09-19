import 'json.dart';

/// overview 里 `summary` 子对象：最近一次样本的摘要（offline/从未上报为 null）。
class SourceSummary {
  const SourceSummary({
    required this.ts,
    this.cpuPercent,
    this.memPercent,
    this.disks = const [],
    this.uptimeSeconds,
  });

  final DateTime ts;
  final double? cpuPercent;
  final double? memPercent;
  final List<DiskUsage> disks;
  final int? uptimeSeconds;

  factory SourceSummary.fromJson(Map<String, dynamic> json) => SourceSummary(
        ts: asDate(json['ts']),
        cpuPercent: asDoubleOrNull(json['cpuPercent']),
        memPercent: asDoubleOrNull(json['memPercent']),
        disks: asMapList(json['disks']).map(DiskUsage.fromJson).toList(),
        uptimeSeconds: asIntOrNull(json['uptimeSeconds']),
      );
}

class DiskUsage {
  const DiskUsage({required this.mount, this.percent});

  final String mount;
  final double? percent;

  factory DiskUsage.fromJson(Map<String, dynamic> json) =>
      DiskUsage(mount: asString(json['mount']), percent: asDoubleOrNull(json['percent']));
}

/// 来源对象。管理视图（/api/v1/sources）字段是超集：enabled / keyHint /
/// attachmentBaseUrl / installHint / createdAt / updatedAt 仅在管理视图出现。
class Source {
  const Source({
    required this.id,
    required this.name,
    required this.kind,
    this.status = 'offline',
    this.lastSeenAt,
    this.agentVersion,
    this.hostname,
    this.capabilities = const {},
    this.summary,
    this.enabled = true,
    this.keyHint,
    this.attachmentBaseUrl,
    this.installHint,
    this.createdAt,
    this.updatedAt,
    this.changeSeq,
  });

  final String id;
  final String name;
  final String kind; // device | feedback
  final String status; // online | offline
  final DateTime? lastSeenAt;
  final String? agentVersion;
  final String? hostname;
  final Map<String, String> capabilities;
  final SourceSummary? summary;
  final bool enabled;
  final String? keyHint;
  final String? attachmentBaseUrl;
  final Object? installHint;
  final DateTime? createdAt;
  final DateTime? updatedAt;
  final int? changeSeq;

  bool get isDevice => kind == 'device';
  bool get isOnline => status == 'online';

  /// 某能力项的呈现：ok / asleep / unsupported / failed / 未知（缺席）。
  String capability(String key) => capabilities[key] ?? 'unknown';

  factory Source.fromJson(Map<String, dynamic> json) => Source(
        id: asString(json['id']),
        name: asString(json['name']),
        kind: asString(json['kind'], 'device'),
        status: asString(json['status'], 'offline'),
        lastSeenAt: asDateOrNull(json['lastSeenAt']),
        agentVersion: asStringOrNull(json['agentVersion']),
        hostname: asStringOrNull(json['hostname']),
        capabilities: asStringMap(json['capabilities']),
        summary:
            json['summary'] == null ? null : SourceSummary.fromJson(asMap(json['summary'])),
        enabled: asBool(json['enabled'], true),
        keyHint: asStringOrNull(json['keyHint']),
        attachmentBaseUrl: asStringOrNull(json['attachmentBaseUrl']),
        installHint: json['installHint'],
        createdAt: asDateOrNull(json['createdAt']),
        updatedAt: asDateOrNull(json['updatedAt']),
        changeSeq: asIntOrNull(json['changeSeq']),
      );

  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'kind': kind,
        'status': status,
        'lastSeenAt': lastSeenAt?.toUtc().toIso8601String(),
        'agentVersion': agentVersion,
        'hostname': hostname,
        'capabilities': capabilities,
        'summary': summary == null
            ? null
            : {
                'ts': summary!.ts.toUtc().toIso8601String(),
                'cpuPercent': summary!.cpuPercent,
                'memPercent': summary!.memPercent,
                'disks': summary!.disks
                    .map((d) => {'mount': d.mount, 'percent': d.percent})
                    .toList(),
                'uptimeSeconds': summary!.uptimeSeconds,
              },
        'enabled': enabled,
        'keyHint': keyHint,
        'attachmentBaseUrl': attachmentBaseUrl,
        'installHint': installHint,
        'createdAt': createdAt?.toUtc().toIso8601String(),
        'updatedAt': updatedAt?.toUtc().toIso8601String(),
        if (changeSeq != null) 'changeSeq': changeSeq,
      };
}
