import 'fault.dart';
import 'json.dart';
import 'settings.dart';
import 'source.dart';

/// `GET /api/v1/overview` 响应：首页一屏数据。
class Overview {
  const Overview({
    required this.serverTime,
    required this.hubReachableHint,
    required this.sources,
    required this.openFaults,
    required this.unreadMessages,
    required this.rulesVersion,
    required this.settingsVersion,
    this.dnd,
  });

  final DateTime serverTime;
  final bool hubReachableHint;
  final List<Source> sources;
  final List<Fault> openFaults;
  final int unreadMessages;
  final int rulesVersion;
  final int settingsVersion;
  final DndSettings? dnd;

  factory Overview.fromJson(Map<String, dynamic> json) => Overview(
        serverTime: asDate(json['serverTime']),
        hubReachableHint: asBool(json['hubReachableHint'], true),
        sources: asMapList(json['sources']).map(Source.fromJson).toList(),
        openFaults: asMapList(json['openFaults']).map(Fault.fromJson).toList(),
        unreadMessages: asInt(json['unreadMessages']),
        rulesVersion: asInt(json['rulesVersion']),
        settingsVersion: asInt(json['settingsVersion']),
        dnd: json['dnd'] == null ? null : DndSettings.fromJson(asMap(json['dnd'])),
      );
}
