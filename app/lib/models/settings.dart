import 'json.dart';

/// 免打扰设置。窗口执行在原生侧；Dart 仅按本地副本显示/编辑。
/// `start`/`end` 为 "HH:mm"，支持跨零点（如 23:00–08:00）。
class DndSettings {
  const DndSettings({
    required this.enabled,
    required this.start,
    required this.end,
    required this.timezone,
  });

  final bool enabled;
  final String start;
  final String end;
  final String timezone;

  static const fallback = DndSettings(
    enabled: false,
    start: '23:00',
    end: '08:00',
    timezone: 'Asia/Shanghai',
  );

  factory DndSettings.fromJson(Map<String, dynamic> json) => DndSettings(
        enabled: asBool(json['enabled']),
        start: asString(json['start'], '23:00'),
        end: asString(json['end'], '08:00'),
        timezone: asString(json['timezone'], 'Asia/Shanghai'),
      );

  Map<String, dynamic> toJson() =>
      {'enabled': enabled, 'start': start, 'end': end, 'timezone': timezone};

  DndSettings copyWith({bool? enabled, String? start, String? end, String? timezone}) =>
      DndSettings(
        enabled: enabled ?? this.enabled,
        start: start ?? this.start,
        end: end ?? this.end,
        timezone: timezone ?? this.timezone,
      );

  /// "HH:mm" → 当天分钟数；非法格式返回 null。
  static int? _minutesOf(String hhmm) {
    final parts = hhmm.split(':');
    if (parts.length != 2) return null;
    final h = int.tryParse(parts[0]);
    final m = int.tryParse(parts[1]);
    if (h == null || m == null || h < 0 || h > 23 || m < 0 || m > 59) return null;
    return h * 60 + m;
  }

  /// 给定（设备本地）时刻是否落在免打扰窗口内。
  ///
  /// - 跨零点窗口（start > end）：t >= start 或 t < end 均为窗口内；
  /// - start == end：视为全天开启；
  /// - 执行在原生侧，这里仅用于 UI 展示，按设备本地时间计算。
  bool isActiveAt(DateTime localTime) {
    if (!enabled) return false;
    final s = _minutesOf(start);
    final e = _minutesOf(end);
    if (s == null || e == null) return false;
    if (s == e) return true;
    final t = localTime.hour * 60 + localTime.minute;
    if (s < e) return t >= s && t < e;
    return t >= s || t < e;
  }
}

/// `GET /api/v1/settings` 响应（`PATCH` 返回同构对象）。
class Settings {
  const Settings({
    required this.version,
    required this.dnd,
    this.reportIntervalSeconds = 30,
  });

  final int version;
  final DndSettings dnd;
  final int reportIntervalSeconds;

  factory Settings.fromJson(Map<String, dynamic> json) => Settings(
        version: asInt(json['version']),
        dnd: DndSettings.fromJson(asMap(json['dnd'])),
        reportIntervalSeconds: asInt(json['reportIntervalSeconds'], 30),
      );
}
