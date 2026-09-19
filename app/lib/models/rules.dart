import 'json.dart';

/// 告警规则（api-v1 §4）。`kind` ∈ threshold | container_exit | smart | pool，
/// 未知 kind 容忍保存（契约允许枚举新增）。
class AlertRule {
  const AlertRule({
    required this.id,
    required this.kind,
    this.metric,
    this.label,
    this.op,
    this.value,
    this.forSeconds,
    this.recoverValue,
    this.recoverForSeconds,
    this.match,
    this.severity = 'warning',
    this.enabled = true,
  });

  final String id;
  final String kind;
  final String? metric; // threshold: cpu_percent | mem_percent | disk_percent
  final String? label; // 标签选择器，null 或 "*" 匹配全部
  final String? op; // gt 等
  final double? value;
  final int? forSeconds;
  final double? recoverValue;
  final int? recoverForSeconds;
  final String? match; // container_exit 用
  final String severity; // warning | critical（规则不产生 info）
  final bool enabled;

  factory AlertRule.fromJson(Map<String, dynamic> json) => AlertRule(
        id: asString(json['id']),
        kind: asString(json['kind'], 'custom'),
        metric: asStringOrNull(json['metric']),
        label: asStringOrNull(json['label']),
        op: asStringOrNull(json['op']),
        value: asDoubleOrNull(json['value']),
        forSeconds: asIntOrNull(json['forSeconds']),
        recoverValue: asDoubleOrNull(json['recoverValue']),
        recoverForSeconds: asIntOrNull(json['recoverForSeconds']),
        match: asStringOrNull(json['match']),
        severity: asString(json['severity'], 'warning'),
        enabled: asBool(json['enabled'], true),
      );

  Map<String, dynamic> toJson() => {
        'id': id,
        'kind': kind,
        if (metric != null) 'metric': metric,
        'label': label,
        if (op != null) 'op': op,
        if (value != null) 'value': value,
        if (forSeconds != null) 'forSeconds': forSeconds,
        if (recoverValue != null) 'recoverValue': recoverValue,
        if (recoverForSeconds != null) 'recoverForSeconds': recoverForSeconds,
        if (match != null) 'match': match,
        'severity': severity,
        'enabled': enabled,
      };

  AlertRule copyWith({
    String? metric,
    String? label,
    bool clearLabel = false,
    double? value,
    int? forSeconds,
    double? recoverValue,
    int? recoverForSeconds,
    String? severity,
    bool? enabled,
  }) =>
      AlertRule(
        id: id,
        kind: kind,
        metric: metric ?? this.metric,
        label: clearLabel ? null : (label ?? this.label),
        op: op,
        value: value ?? this.value,
        forSeconds: forSeconds ?? this.forSeconds,
        recoverValue: recoverValue ?? this.recoverValue,
        recoverForSeconds: recoverForSeconds ?? this.recoverForSeconds,
        match: match,
        severity: severity ?? this.severity,
        enabled: enabled ?? this.enabled,
      );
}

/// `GET /api/v1/rules` 响应（`PUT` 返回同构对象）。
class Rules {
  const Rules({
    required this.version,
    required this.heartbeatSeconds,
    required this.rules,
  });

  final int version;
  final int heartbeatSeconds; // 60..3600
  final List<AlertRule> rules;

  factory Rules.fromJson(Map<String, dynamic> json) => Rules(
        version: asInt(json['version']),
        heartbeatSeconds: asInt(json['heartbeatSeconds'], 180),
        rules: asMapList(json['rules']).map(AlertRule.fromJson).toList(),
      );
}
