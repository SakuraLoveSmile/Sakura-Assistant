import 'dart:async';

import 'package:connectivity_plus/connectivity_plus.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../api/api_client.dart';
import '../api/api_exception.dart';
import '../bridge/native_bridge.dart';
import '../cache/assistant_cache.dart';
import '../models/service_state.dart';
import '../models/session.dart';
import '../models/settings.dart';
import 'session.dart';

/// main() 注入的已加载偏好。
final sharedPrefsProvider = Provider<SharedPreferences>(
  (ref) => throw UnimplementedError('sharedPrefsProvider 未被覆盖'),
);

final tokenStoreProvider = Provider<TokenStore>(
  (ref) => SecureTokenStore(ref.watch(sharedPrefsProvider)),
);

final nativeBridgeProvider = Provider<NativeBridge>(
  (ref) => MethodChannelNativeBridge(),
);

/// main() 冷启动读到的通知跳转目标（consumePendingRoute 兜底另行拉取）。
final launchPayloadProvider = Provider<LaunchPayload?>((ref) => null);

/// main() 启动时已恢复的会话（无则 null）。
final initialSessionProvider = Provider<Session?>((ref) => null);

/// 已打开的缓存库（main() 打开后覆盖）。
final cacheProvider = Provider<AssistantCache>(
  (ref) => throw UnimplementedError('cacheProvider 未被覆盖'),
);

/// 会话状态：null = 未登录。
final sessionProvider = NotifierProvider<SessionNotifier, Session?>(
  SessionNotifier.new,
);

class SessionNotifier extends Notifier<Session?> {
  @override
  Session? build() => ref.watch(initialSessionProvider);

  /// 登录：调 API → 存 token → 绑定会话 → 配置原生桥 + 启动服务 + 全量 sync。
  Future<Session> login({
    required String hubUrl,
    required String username,
    required String password,
    String? deviceLabel,
  }) async {
    final api = ref.read(apiProvider);
    final normalized = normalizeHubUrl(hubUrl);
    final result = await api.login(
      hubUrl: normalized,
      username: username,
      password: password,
      deviceLabel: deviceLabel,
    );
    final session = Session(
      hubUrl: normalized,
      token: result.token,
      refreshToken: result.refreshToken,
      username: result.username,
      expiresAt: result.expiresAt,
    );
    api.bindSession(session);
    await ref.read(tokenStoreProvider).save(session);
    state = session;
    unawaited(_afterAuth());
    return session;
  }

  /// 登录后 / 冷启动 / 设置变更后：configure 原生桥 + startService + sync。
  /// 幂等、尽力而为——任何一步失败都不影响 UI 主流程。
  Future<void> _afterAuth() async {
    if (state == null) return;
    await _pushConfigToNative();
    await ref.read(nativeBridgeProvider).startService();
    unawaited(
        ref.read(syncCoordinatorProvider).syncAll().catchError((_) => 0));
  }

  /// 把当前会话凭证 + 最新设置下发给原生服务。
  /// refreshToken 为一次性轮换值：本端每次刷新后都必须重新下发，
  /// 否则原生侧旧凭证自愈必败（401 → invalid_refresh → authExpired）。
  Future<void> _pushConfigToNative() async {
    if (state == null) return;
    var reportInterval = 30;
    var dnd = DndSettings.fallback;
    try {
      final settings = await ref.read(apiProvider).getSettings();
      reportInterval = settings.reportIntervalSeconds;
      dnd = settings.dnd;
    } catch (_) {
      try {
        final cached = await ref.read(cacheProvider).settings();
        if (cached != null) {
          reportInterval = cached.reportIntervalSeconds;
          dnd = cached.dnd;
        }
      } catch (_) {}
    }
    // 设置拉取期间会话可能已被刷新 / 收养，取最新值再下发。
    final s = state;
    if (s == null) return;
    await ref.read(nativeBridgeProvider).configure(
          hubUrl: s.hubUrl,
          token: s.token,
          refreshToken: s.refreshToken,
          reportIntervalSeconds: reportInterval,
          dnd: dnd,
        );
  }

  /// 设置（dnd/reportInterval）变更后重新下发原生配置。
  Future<void> pushNativeConfig({
    required int reportIntervalSeconds,
    required DndSettings dnd,
  }) async {
    final s = state;
    if (s == null) return;
    await ref.read(nativeBridgeProvider).configure(
          hubUrl: s.hubUrl,
          token: s.token,
          refreshToken: s.refreshToken,
          reportIntervalSeconds: reportIntervalSeconds,
          dnd: dnd,
        );
  }

  /// token 刷新成功后由 api 回调：更新内存态 + 持久化 + 下发原生。
  void onSessionRefreshed(Session session) {
    state = session;
    unawaited(ref.read(tokenStoreProvider).save(session));
    // 旧 refreshToken 已随轮换失效，必须立即同步原生，否则通知通道将静默死亡。
    unawaited(_pushConfigToNative());
  }

  /// 原生侧 401 自愈轮换后的凭证回写（session 事件）：保持双端凭证一致。
  void onNativeSession(String token, String refreshToken) {
    final s = state;
    if (s == null ||
        (s.token == token && s.refreshToken == refreshToken) ||
        token.isEmpty) {
      return;
    }
    final next = s.copyWith(token: token, refreshToken: refreshToken);
    ref.read(apiProvider).bindSession(next);
    state = next;
    unawaited(ref.read(tokenStoreProvider).save(next));
  }

  /// 冷启动契约：恢复会话后必须先与本端/原生凭证对齐，再 configure + startService。
  /// Dart 休眠期间原生可能已轮换 refreshToken —— 本端校验失败时收养原生侧而非登出。
  Future<void> reconcileColdStart() async {
    if (state == null) return;
    // 先用本端会话实调一次（api 内部已含 401→refresh 自愈；成功即本端凭证有效）。
    var dartValid = true;
    try {
      await ref.read(apiProvider).getSettings();
    } on ApiException catch (e) {
      if (e.isUnauthorized) dartValid = false;
    } catch (_) {
      // 网络 / 中枢不可达：凭证未必失效，照旧按本端配置下发。
    }
    if (!dartValid) {
      try {
        final ns = await ref.read(nativeBridgeProvider).getSession();
        final s = state;
        if (s != null &&
            ns != null &&
            ns.refreshToken.isNotEmpty &&
            ns.refreshToken != s.refreshToken) {
          onNativeSession(ns.token, ns.refreshToken);
        }
      } catch (_) {}
      // 收养后仍未生效（state 被 handleExpired 清空除外）→ 交给既有流程兜底。
      if (state == null) return;
    }
    await _afterAuth();
  }

  /// refresh 失败 → 先尝试收养原生侧凭证（Dart 休眠期原生可能已轮换）；
  /// 两侧一致或原生无凭证才本地登出（回登录页由路由 redirect 处理）。
  Future<void> handleExpired() async {
    final s = state;
    try {
      final ns = await ref.read(nativeBridgeProvider).getSession();
      if (s != null &&
          ns != null &&
          ns.refreshToken.isNotEmpty &&
          ns.refreshToken != s.refreshToken) {
        onNativeSession(ns.token, ns.refreshToken);
        return;
      }
    } catch (_) {}
    await _clearLocal(stopService: true);
  }

  Future<void> logout() async {
    final api = ref.read(apiProvider);
    await api.logout(); // 尽力撤销，不阻塞
    await _clearLocal(stopService: true);
  }

  Future<void> _clearLocal({required bool stopService}) async {
    ref.read(apiProvider).bindSession(null);
    await ref.read(tokenStoreProvider).clear();
    if (stopService) {
      await ref.read(nativeBridgeProvider).stopService();
    }
    state = null;
  }
}

/// 全局 REST client（Fake 可在测试替换）。
final apiProvider = Provider<AssistantApi>((ref) {
  final api = HttpAssistantApi(
    onSessionRefreshed: (s) =>
        ref.read(sessionProvider.notifier).onSessionRefreshed(s),
    onSessionExpired: () =>
        unawaited(ref.read(sessionProvider.notifier).handleExpired()),
  );
  api.bindSession(ref.read(sessionProvider));
  ref.onDispose(() => api.bindSession(null));
  return api;
});

// ---------------- 网络连通 ----------------

/// 手机是否联网（区分「手机断网」与「中枢不可达」）。
abstract class ConnectivityMonitor {
  Future<bool> isOnline();

  Stream<bool> get onlineStream;
}

class ConnectivityPlusMonitor implements ConnectivityMonitor {
  ConnectivityPlusMonitor([Connectivity? connectivity])
      : _c = connectivity ?? Connectivity();

  final Connectivity _c;

  static bool _any(List<ConnectivityResult> results) =>
      results.any((r) => r != ConnectivityResult.none);

  @override
  Future<bool> isOnline() async {
    try {
      return _any(await _c.checkConnectivity());
    } catch (_) {
      return true; // 插件缺失：不显示断网横幅
    }
  }

  @override
  Stream<bool> get onlineStream {
    try {
      return _c.onConnectivityChanged
          .map(_any)
          .handleError((_) => true);
    } catch (_) {
      return const Stream<bool>.empty();
    }
  }
}

final connectivityMonitorProvider = Provider<ConnectivityMonitor>(
  (ref) => ConnectivityPlusMonitor(),
);

/// 当前是否联网（默认 true，插件/平台异常时不误报断网）。
final onlineProvider = StreamProvider<bool>((ref) async* {
  final monitor = ref.watch(connectivityMonitorProvider);
  yield await monitor.isOnline();
  yield* monitor.onlineStream;
});

// ---------------- 缓存变更计数 ----------------

/// 缓存写入/同步后 bump，让基于缓存的 provider 重新计算。
final cacheRevisionProvider = NotifierProvider<CacheRevision, int>(
  CacheRevision.new,
);

class CacheRevision extends Notifier<int> {
  @override
  int build() => 0;

  void bump() => state++;
}

// ---------------- sync 协调 ----------------

final syncCoordinatorProvider = Provider<SyncCoordinator>(
  (ref) => SyncCoordinator(ref),
);

/// 追平本地缓存：从上次 cursor 起循环拉 sync 直到 hasMore=false。
class SyncCoordinator {
  SyncCoordinator(this._ref);

  final Ref _ref;
  bool _running = false;

  Future<int> syncAll() async {
    if (_running) return -1;
    _running = true;
    try {
      final api = _ref.read(apiProvider);
      if (api.session == null) return -1;
      final cache = _ref.read(cacheProvider);
      var cursor = await cache.cursor;
      var guard = 0;
      while (true) {
        final page = await api.getSync(since: cursor, limit: 200);
        await cache.applySyncPage(page);
        cursor = page.cursor;
        if (!page.hasMore || ++guard >= 100) break;
      }
      _ref.read(cacheRevisionProvider.notifier).bump();
      return cursor;
    } finally {
      _running = false;
    }
  }
}

// ---------------- 原生服务状态 ----------------

/// 服务状态：周期轮询 getServiceState + EventChannel service 主动推送。
final serviceStateProvider = StreamProvider<ServiceState>((ref) {
  final bridge = ref.watch(nativeBridgeProvider);
  late StreamController<ServiceState> controller;
  Timer? timer;
  StreamSubscription<NativeEvent>? sub;

  Future<void> poll() async {
    final state = await bridge.getServiceState();
    if (!controller.isClosed) controller.add(state);
  }

  controller = StreamController<ServiceState>(
    onListen: () {
      unawaited(poll());
      timer = Timer.periodic(const Duration(seconds: 15), (_) => unawaited(poll()));
      sub = bridge.events
          .where((e) => e is ServiceEvent)
          .cast<ServiceEvent>()
          .listen(
            (e) => controller.add(e.state),
            onError: (_) {},
          );
    },
    onCancel: () {
      timer?.cancel();
      sub?.cancel();
    },
  );
  ref.onDispose(() {
    timer?.cancel();
    sub?.cancel();
    unawaited(controller.close());
  });
  return controller.stream;
});
