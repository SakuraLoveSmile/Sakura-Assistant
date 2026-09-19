import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/date_symbol_data_local.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'app.dart';
import 'bridge/native_bridge.dart';
import 'cache/assistant_cache.dart';
import 'state/providers.dart';
import 'state/session.dart';

/// 启动流程：
/// 1. 加载偏好 + 安全存储里的会话；
/// 2. 打开本地缓存库；
/// 3. 冷启动读取原生 getLaunchPayload（通知点开的跳转目标）；
/// 4. 全部注入 ProviderScope 后 runApp；
/// 5. 已登录会话触发 reconcileColdStart（bridge.md：冷启动必须 configure）。
Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await initializeDateFormatting('zh_CN');

  final prefs = await SharedPreferences.getInstance();
  final tokenStore = SecureTokenStore(prefs);
  final session = await tokenStore.load();
  final cache = await AssistantCache.open();
  final bridge = MethodChannelNativeBridge();
  // 冷启动通知点击目标（取一次即清除）。
  final launchPayload = await bridge.getLaunchPayload();

  final container = ProviderContainer(
    overrides: [
      sharedPrefsProvider.overrideWithValue(prefs),
      tokenStoreProvider.overrideWithValue(tokenStore),
      nativeBridgeProvider.overrideWithValue(bridge),
      cacheProvider.overrideWithValue(cache),
      initialSessionProvider.overrideWithValue(session),
      launchPayloadProvider.overrideWithValue(launchPayload),
    ],
  );

  // 契约要求冷启动必调 configure；reconcile 同时负责双端凭证对齐
  // （Dart 休眠期原生侧可能已轮换 refreshToken）。异步执行不阻塞首帧。
  if (session != null) {
    unawaited(
      container.read(sessionProvider.notifier).reconcileColdStart(),
    );
  }

  runApp(
    UncontrolledProviderScope(
      container: container,
      child: const AssistantApp(),
    ),
  );
}
