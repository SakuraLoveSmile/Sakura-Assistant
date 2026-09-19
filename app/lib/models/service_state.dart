import 'json.dart';

/// 原生前台服务状态（bridge `getServiceState` / EventChannel `service` 事件）。
class ServiceState {
  const ServiceState({
    this.running = false,
    this.hubReachable = false,
    this.lastSyncAt,
    this.lastChangeSeq = 0,
    this.dndActive = false,
    this.heldCount = 0,
    this.authExpired = false,
    this.lastError,
  });

  final bool running;
  final bool hubReachable;
  final DateTime? lastSyncAt;
  final int lastChangeSeq;
  final bool dndActive;
  final int heldCount;

  /// refresh 已失效、需重新登录（bridge.md §3 invalid_refresh）。
  final bool authExpired;
  final String? lastError;

  static const stopped = ServiceState();

  factory ServiceState.fromJson(Map<String, dynamic> json) => ServiceState(
        running: asBool(json['running']),
        hubReachable: asBool(json['hubReachable']),
        lastSyncAt: asDateOrNull(json['lastSyncAt']),
        lastChangeSeq: asInt(json['lastChangeSeq']),
        dndActive: asBool(json['dndActive']),
        heldCount: asInt(json['heldCount']),
        authExpired: asBool(json['authExpired']),
        lastError: asStringOrNull(json['lastError']),
      );
}

/// 通知点击的跳转目标（`getLaunchPayload` / `consumePendingRoute` / launch 事件）。
class LaunchPayload {
  const LaunchPayload({required this.route, this.id});

  /// message | fault | home
  final String route;
  final String? id;

  factory LaunchPayload.fromJson(Map<String, dynamic> json) => LaunchPayload(
        route: asString(json['route'], 'home'),
        id: asStringOrNull(json['id']),
      );
}

/// `getDeviceInfo` 返回的设备信息，用于权限引导文案定制。
class DeviceInfo {
  const DeviceInfo({
    this.manufacturer = '',
    this.model = '',
    this.sdkInt = 0,
    this.miui,
  });

  final String manufacturer;
  final String model;
  final int sdkInt;
  final String? miui;

  bool get isMiui => (miui != null && miui!.isNotEmpty) ||
      manufacturer.toLowerCase().contains('xiaomi');

  factory DeviceInfo.fromJson(Map<String, dynamic> json) => DeviceInfo(
        manufacturer: asString(json['manufacturer']),
        model: asString(json['model']),
        sdkInt: asInt(json['sdkInt']),
        miui: asStringOrNull(json['miui']),
      );
}
