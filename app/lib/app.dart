import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_localizations/flutter_localizations.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import 'bridge/native_bridge.dart';
import 'feedback/feedback_host.dart';
import 'state/providers.dart';
import 'state/router.dart';
import 'theme/app_theme.dart';

/// 应用根：MaterialApp.router + 中文本地化 + 明暗双主题。
/// 同时订阅原生 launch 事件做热态通知跳转。
class AssistantApp extends ConsumerStatefulWidget {
  const AssistantApp({super.key});

  @override
  ConsumerState<AssistantApp> createState() => _AssistantAppState();
}

class _AssistantAppState extends ConsumerState<AssistantApp>
    with WidgetsBindingObserver {
  StreamSubscription<NativeEvent>? _eventSub;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    final bridge = ref.read(nativeBridgeProvider);
    _eventSub = bridge.events.listen(_onNativeEvent, onError: (_) {});
    // 热启动兜底：事件通道没及时送达的 pending route。
    WidgetsBinding.instance.addPostFrameCallback((_) => _consumePending());
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    _eventSub?.cancel();
    super.dispose();
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == AppLifecycleState.resumed) {
      unawaited(_consumePending());
    }
  }

  Future<void> _consumePending() async {
    final bridge = ref.read(nativeBridgeProvider);
    final payload = await bridge.consumePendingRoute();
    if (payload != null) _navigate(payload.route, payload.id);
  }

  void _onNativeEvent(NativeEvent event) {
    switch (event) {
      case LaunchEvent():
        _navigate(event.payload.route, event.payload.id);
      case SessionEvent():
        // 原生侧 401 自愈轮换 → 回写本端会话（refreshToken 一次性失效旧值）。
        ref
            .read(sessionProvider.notifier)
            .onNativeSession(event.token, event.refreshToken);
      case ServiceEvent():
      case SummaryEvent():
        break; // 服务态由 serviceStateProvider 轮询+订阅处理
    }
  }

  void _navigate(String route, String? id) {
    final router = ref.read(routerProvider);
    switch (route) {
      case 'message':
        if (id != null) {
          router.push('/messages/${Uri.encodeComponent(id)}');
        }
      case 'fault':
        if (id != null) {
          router.push('/faults/${Uri.encodeComponent(id)}');
        }
      default:
        router.go('/');
    }
  }

  @override
  Widget build(BuildContext context) {
    final router = ref.watch(routerProvider);
    return MaterialApp.router(
      title: 'Assistant',
      debugShowCheckedModeBanner: false,
      theme: AppTheme.light(),
      darkTheme: AppTheme.dark(),
      themeMode: ThemeMode.system,
      routerConfig: router,
      // 反馈提交组件包在应用 Navigator 之外：路由切换不丢草稿 / 登录态，
      // 且面板可在任意路由上呼出（结构同组件官方示例）。
      builder: (context, child) => FeedbackHost(child: child),
      localizationsDelegates: const [
        GlobalMaterialLocalizations.delegate,
        GlobalWidgetsLocalizations.delegate,
        GlobalCupertinoLocalizations.delegate,
      ],
      supportedLocales: const [Locale('zh', 'CN'), Locale('en')],
      locale: const Locale('zh', 'CN'),
    );
  }
}
