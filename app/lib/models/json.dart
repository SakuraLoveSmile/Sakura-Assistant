/// 容错 JSON 解析工具。
///
/// 契约约定「新增字段必须容忍、缺失字段不崩」：所有 model 的 fromJson 只读取
/// 已知字段，未知键自动忽略；缺失/类型不符时回退默认值而不是抛异常。
library;

/// 把任意值宽松地转成 Map；不是 Map 时返回空 Map。
Map<String, dynamic> asMap(Object? value) {
  if (value is Map<String, dynamic>) return value;
  if (value is Map) {
    return value.map((k, v) => MapEntry(k.toString(), v));
  }
  return <String, dynamic>{};
}

/// 宽松转 List；不是 List 时返回空 List。
List<dynamic> asList(Object? value) {
  if (value is List) return value;
  return const <dynamic>[];
}

/// 宽松转 List<Map>。
List<Map<String, dynamic>> asMapList(Object? value) {
  return asList(value).map(asMap).toList();
}

String? asStringOrNull(Object? value) {
  if (value == null) return null;
  if (value is String) return value;
  return value.toString();
}

String asString(Object? value, [String fallback = '']) {
  return asStringOrNull(value) ?? fallback;
}

int? asIntOrNull(Object? value) {
  if (value is int) return value;
  if (value is num) return value.toInt();
  if (value is String) return int.tryParse(value);
  return null;
}

int asInt(Object? value, [int fallback = 0]) {
  return asIntOrNull(value) ?? fallback;
}

double? asDoubleOrNull(Object? value) {
  if (value is double) return value;
  if (value is num) return value.toDouble();
  if (value is String) return double.tryParse(value);
  return null;
}

bool asBool(Object? value, [bool fallback = false]) {
  if (value is bool) return value;
  if (value is num) return value != 0;
  if (value is String) {
    final lower = value.toLowerCase();
    if (lower == 'true') return true;
    if (lower == 'false') return false;
  }
  return fallback;
}

/// RFC3339（UTC）解析；解析失败返回 null 而不是抛异常。
DateTime? asDateOrNull(Object? value) {
  if (value is DateTime) return value;
  if (value is String && value.isNotEmpty) {
    return DateTime.tryParse(value);
  }
  return null;
}

DateTime asDate(Object? value, [DateTime? fallback]) {
  return asDateOrNull(value) ?? fallback ?? DateTime.fromMillisecondsSinceEpoch(0);
}

/// `Map<String, String>`（capabilities 之类的动态键值对），容忍非 String 值。
Map<String, String> asStringMap(Object? value) {
  final map = asMap(value);
  return map.map((k, v) => MapEntry(k, asString(v)));
}
