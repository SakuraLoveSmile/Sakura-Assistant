import 'dart:async';

import 'package:flutter/services.dart';

import '../models/json.dart';
import '../models/service_state.dart';
import '../models/settings.dart';

/// EventChannel `assistant/native_events` 推送的事件。
sealed class NativeEvent {
  const NativeEvent();
}

/// 热态通知点击（`{"type":"launch","route":"message","id":"msg_…"}`）。
class LaunchEvent extends NativeEvent {
  const LaunchEvent(this.payload);

  final LaunchPayload payload;
}

/// 服务状态主动推送（`{"type":"service","state":{…}}`）。
class ServiceEvent extends NativeEvent {
  const ServiceEvent(this.state);

  final ServiceState state;
}

/// DND 结束汇总已发出（`{"type":"summary","delivered":5}`）。
class SummaryEvent extends NativeEvent {
  const SummaryEvent(this.delivered);

  final int delivered;
}

/// 原生侧 401 自愈轮换后的凭证回写（`{"type":"session","token":…,"refreshToken":…}`）。
class SessionEvent extends NativeEvent {
  const SessionEvent(this.token, this.refreshToken);

  final String token;
  final String refreshToken;
}

/// 原生侧当前持有的会话凭证（getSession 拉取）。
/// refreshToken 为一次性轮换值，双端任一刷新都会使另一端的副本失效，
/// 因此冷启动 / 本端 refresh 失败时需以此为准对齐。
class NativeSession {
  const NativeSession({required this.token, required this.refreshToken});

  final String token;
  final String refreshToken;
}

/// Flutter ↔ Kotlin 桥接抽象。
///
/// 契约要求：原生侧缺失（MissingPluginException）或调用失败（PlatformException）
/// 时必须容错——视为「服务未运行 / 无 launch payload / 设置页跳转失败」，绝不崩溃。
abstract class NativeBridge {
  /// 写原生配置副本（登录 / 设置变更 / 冷启动都调，幂等）。
  Future<void> configure({
    required String hubUrl,
    required String token,
    required String refreshToken,
    required int reportIntervalSeconds,
    required DndSettings dnd,
  });

  Future<ServiceState> getServiceState();

  /// 原生侧当前持有的会话凭证；未配置 / token 为空返回 null。
  Future<NativeSession?> getSession();

  /// 启动前台服务；返回是否运行（原生缺失时返回 false）。
  Future<bool> startService();

  Future<bool> stopService();

  /// 冷启动跳转目标，取一次即清除。
  Future<LaunchPayload?> getLaunchPayload();

  /// 热启动兜底拉取。
  Future<LaunchPayload?> consumePendingRoute();

  /// 系统通知设置页；不可达返回 false。
  Future<bool> openNotificationSettings();

  /// 「忽略电池优化」授权页。
  Future<bool> openBatteryOptimizationSettings();

  /// 尽力跳 MIUI 自启管理页。
  Future<bool> openAutostartSettings();

  Future<DeviceInfo> getDeviceInfo();

  Future<void> setDebugLogging({required bool enabled});

  /// 原生 → Dart 事件流（launch / service / summary）。原生缺失时为空流。
  Stream<NativeEvent> get events;
}

/// MethodChannel `assistant/native` + EventChannel `assistant/native_events` 实现。
class MethodChannelNativeBridge implements NativeBridge {
  MethodChannelNativeBridge({
    MethodChannel? methodChannel,
    EventChannel? eventChannel,
  })  : _method = methodChannel ?? const MethodChannel('assistant/native'),
        _events = eventChannel ?? const EventChannel('assistant/native_events');

  final MethodChannel _method;
  final EventChannel _events;

  Future<T?> _invoke<T>(String method, [Object? args]) async {
    try {
      return await _method.invokeMethod<T>(method, args);
    } on MissingPluginException {
      return null; // 原生侧未实现：容错
    } on PlatformException {
      return null;
    }
  }

  @override
  Future<void> configure({
    required String hubUrl,
    required String token,
    required String refreshToken,
    required int reportIntervalSeconds,
    required DndSettings dnd,
  }) =>
      _invoke('configure', {
        'hubUrl': hubUrl,
        'token': token,
        'refreshToken': refreshToken,
        'reportIntervalSeconds': reportIntervalSeconds,
        'dnd': dnd.toJson(),
      });

  @override
  Future<ServiceState> getServiceState() async {
    final res = await _invoke<Object>('getServiceState');
    if (res == null) return ServiceState.stopped;
    return ServiceState.fromJson(asMap(res));
  }

  @override
  Future<bool> startService() async {
    final res = await _invoke<Object>('startService');
    return asBool(asMap(res)['running']);
  }

  @override
  Future<bool> stopService() async {
    final res = await _invoke<Object>('stopService');
    // 契约返回 {"running": false}；原生缺失也视为已停。
    final map = asMap(res);
    return !(asBool(map['running'], true));
  }

  @override
  Future<NativeSession?> getSession() async {
    final res = await _invoke<Object>('getSession');
    if (res == null) return null;
    final map = asMap(res);
    final token = asString(map['token']);
    if (token.isEmpty) return null;
    return NativeSession(token: token, refreshToken: asString(map['refreshToken']));
  }

  @override
  Future<LaunchPayload?> getLaunchPayload() async {
    final res = await _invoke<Object>('getLaunchPayload');
    if (res == null) return null;
    final map = asMap(res);
    if (map.isEmpty) return null;
    return LaunchPayload.fromJson(map);
  }

  @override
  Future<LaunchPayload?> consumePendingRoute() async {
    final res = await _invoke<Object>('consumePendingRoute');
    if (res == null) return null;
    final map = asMap(res);
    if (map.isEmpty) return null;
    return LaunchPayload.fromJson(map);
  }

  @override
  Future<bool> openNotificationSettings() async =>
      asBool(await _invoke<Object>('openNotificationSettings'));

  @override
  Future<bool> openBatteryOptimizationSettings() async =>
      asBool(await _invoke<Object>('openBatteryOptimizationSettings'));

  @override
  Future<bool> openAutostartSettings() async =>
      asBool(await _invoke<Object>('openAutostartSettings'));

  @override
  Future<DeviceInfo> getDeviceInfo() async {
    final res = await _invoke<Object>('getDeviceInfo');
    return res == null ? const DeviceInfo() : DeviceInfo.fromJson(asMap(res));
  }

  @override
  Future<void> setDebugLogging({required bool enabled}) =>
      _invoke('setDebugLogging', {'enabled': enabled});

  @override
  Stream<NativeEvent> get events {
    try {
      return _events.receiveBroadcastStream().map((event) {
        final map = asMap(event);
        switch (asString(map['type'])) {
          case 'launch':
            return LaunchEvent(LaunchPayload.fromJson(map));
          case 'service':
            return ServiceEvent(ServiceState.fromJson(asMap(map['state'])));
          case 'summary':
            return SummaryEvent(asInt(map['delivered']));
          case 'session':
            return SessionEvent(
                asString(map['token']), asString(map['refreshToken']));
          default:
            return null;
        }
      }).where((e) => e != null).cast<NativeEvent>().handleError((_) {
        // 事件流错误（原生侧异常）：吞掉，不向上传播。
      });
    } catch (_) {
      return const Stream<NativeEvent>.empty();
    }
  }
}
