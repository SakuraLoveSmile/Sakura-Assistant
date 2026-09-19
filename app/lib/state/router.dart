import 'package:flutter/foundation.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../models/service_state.dart';
import '../pages/fault_detail_page.dart';
import '../pages/feedback_detail_page.dart';
import '../pages/feedback_list_page.dart';
import '../pages/history_page.dart';
import '../pages/home_page.dart';
import '../pages/login_page.dart';
import '../pages/message_detail_page.dart';
import '../pages/rules_page.dart';
import '../pages/settings_page.dart';
import '../pages/sources_page.dart';
import '../pages/trends_page.dart';
import 'providers.dart';

/// 通知点击的跳转目标 → 路由路径。
String locationForLaunch(LaunchPayload? payload) {
  switch (payload?.route) {
    case 'message':
      final id = payload!.id;
      return id == null ? '/' : '/messages/${Uri.encodeComponent(id)}';
    case 'fault':
      final id = payload!.id;
      return id == null ? '/' : '/faults/${Uri.encodeComponent(id)}';
    default:
      return '/';
  }
}

/// go_router 实例。会话变化经 refreshListenable 触发 redirect 重评估。
final routerProvider = Provider<GoRouter>((ref) {
  final refresh = ValueNotifier<int>(0);
  final sub = ref.listen(sessionProvider, (_, _) => refresh.value++);
  ref.onDispose(() {
    sub.close();
    refresh.dispose();
  });
  final initial = locationForLaunch(ref.read(launchPayloadProvider));
  return GoRouter(
    initialLocation: initial,
    refreshListenable: refresh,
    redirect: (context, state) {
      final loggedIn = ref.read(sessionProvider) != null;
      final onLogin = state.matchedLocation == '/login';
      if (!loggedIn && !onLogin) return '/login';
      if (loggedIn && onLogin) return '/';
      return null;
    },
    routes: [
      GoRoute(path: '/login', builder: (_, _) => const LoginPage()),
      GoRoute(path: '/', builder: (_, _) => const HomePage()),
      GoRoute(
        path: '/history',
        builder: (_, state) => HistoryPage(
          filter: state.uri.queryParameters['filter'],
          sourceId: state.uri.queryParameters['source'],
        ),
      ),
      GoRoute(
        path: '/messages/:id',
        builder: (_, state) =>
            MessageDetailPage(messageId: state.pathParameters['id']!),
      ),
      GoRoute(
        path: '/faults/:id',
        builder: (_, state) =>
            FaultDetailPage(faultId: state.pathParameters['id']!),
      ),
      GoRoute(
        path: '/trends/:sourceId',
        builder: (_, state) => TrendsPage(
          sourceId: state.pathParameters['sourceId']!,
          sourceName: state.uri.queryParameters['name'],
        ),
      ),
      GoRoute(
        path: '/feedback',
        builder: (_, state) => FeedbackListPage(
          initialSourceId: state.uri.queryParameters['source'],
        ),
      ),
      GoRoute(
        path: '/sources/:srcId/feedback/:fbId',
        builder: (_, state) => FeedbackDetailPage(
          sourceId: state.pathParameters['srcId']!,
          feedbackId: state.pathParameters['fbId']!,
        ),
      ),
      GoRoute(path: '/sources', builder: (_, _) => const SourcesPage()),
      GoRoute(path: '/settings', builder: (_, _) => const SettingsPage()),
      GoRoute(path: '/settings/rules', builder: (_, _) => const RulesPage()),
    ],
  );
});
