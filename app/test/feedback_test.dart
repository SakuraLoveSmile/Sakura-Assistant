import 'dart:convert';
import 'dart:io';

import 'package:assistant/api/api_client.dart';
import 'package:assistant/api/api_exception.dart';
import 'package:assistant/models/fault.dart';
import 'package:assistant/models/feedback.dart';
import 'package:assistant/models/message.dart';
import 'package:assistant/models/metrics.dart';
import 'package:assistant/models/overview.dart';
import 'package:assistant/models/rules.dart';
import 'package:assistant/models/session.dart';
import 'package:assistant/models/settings.dart';
import 'package:assistant/models/source.dart';
import 'package:assistant/models/sync.dart';
import 'package:assistant/pages/feedback_detail_page.dart';
import 'package:assistant/pages/feedback_list_page.dart';
import 'package:assistant/state/feedback_providers.dart';
import 'package:assistant/state/providers.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:go_router/go_router.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart' as http_testing;

/// 契约 fixtures（contracts/ 冻结，只读引用）。
Map<String, dynamic> _fixture(String name) {
  final file = File('../contracts/fixtures/$name');
  return asMapFixture(jsonDecode(file.readAsStringSync()));
}

Map<String, dynamic> asMapFixture(Object? v) =>
    (v as Map).map((k, e) => MapEntry(k.toString(), e));

Session _session() => Session(
      hubUrl: 'http://hub.test',
      token: 'tok',
      refreshToken: 'ref',
      username: 'u',
    );

void main() {
  group('反馈模型解析（契约 fixtures）', () {
    test('列表：items / counts / nextCursor / 管理字段', () {
      final json = _fixture('feedback-list.json');
      final items = (json['items'] as List)
          .map((e) => FeedbackItem.fromJson(asMapFixture(e)))
          .toList();
      expect(items, hasLength(2));

      final first = items.first;
      expect(first.id, '0198f3a2-7c4e-7b2d-9e5f-1a2b3c4d5e6f');
      expect(first.appId, 'com.sakurasep.assistant');
      expect(first.status, 'archived');
      expect(first.issueStatus, 'open');
      expect(first.mgmtState, 'inbox');
      expect(first.lifecycleVersion, 0);
      expect(first.title, '首页趋势图偶发不刷新');
      expect(first.hasScreenshot, true);
      expect(first.logCount, 1);
      expect(first.collectionState, 'queued');
      expect(first.resumePaused, false);
      expect(first.allowedActions, ['archive', 'trash']);
      expect(first.errorSummary, isNull);
      expect(first.createdAt!.isUtc, true);

      // title 缺省 → displayTitle 回退 textPreview。
      final second = items[1];
      expect(second.title, isNull);
      expect(second.displayTitle, '通知点了没反应');
      expect(second.errorSummary, isNotNull);

      // 顶层字段。
      expect(json['nextCursor'], isNotNull);
      final counts =
          asMapFixture(json['counts']).map((k, v) => MapEntry(k, v as int));
      expect(counts, {'inbox': 2, 'archived': 12, 'trash': 1});
    });

    test('详情：全部 v1.1 管理字段 + logs + capabilities', () {
      final d = FeedbackDetail.fromJson(_fixture('feedback-detail.json'));
      expect(d.id, '0198f3a2-7c4e-7b2d-9e5f-1a2b3c4d5e6f');
      expect(d.status, 'archived');
      expect(d.text, contains('趋势图'));
      expect(d.mgmtState, 'inbox');
      expect(d.lifecycleVersion, 0);
      expect(d.revision, 2);
      expect(d.issueStatus, 'open');
      expect(d.collectionState, 'queued');
      expect(d.archiveStage, 'complete');
      expect(d.kaneoTaskUrl, 'https://kaneo.example.com/tasks/417');
      expect(d.manage, true);
      expect(d.hasMgmtFields, true);
      expect(d.logs, hasLength(1));
      expect(d.logs.first.filename, 'assistant.log');
      expect(d.logs.first.byteSize, 18234);
      expect(d.knownActions,
          [FeedbackAction.archive, FeedbackAction.trash]);
      expect(d.sourceId, 'src_01J8WEXAMPLE000000000000');
      expect(d.sourceName, 'Feedback');
    });

    test('旧版服务端详情：管理字段全缺席 → 容忍且不视为可管理', () {
      final d = FeedbackDetail.fromJson(<String, dynamic>{
        'id': 'fb1',
        'status': 'archived',
        'text': 'old server',
      });
      expect(d.mgmtState, isNull);
      expect(d.lifecycleVersion, isNull);
      expect(d.capabilities, isEmpty);
      expect(d.manage, false);
      expect(d.hasMgmtFields, false);
      expect(d.allowedActions, isEmpty);
      expect(d.knownActions, isEmpty);
      expect(d.displayTitle, 'old server');
    });

    test('空对象 / 未知字段：不抛异常走默认', () {
      final item = FeedbackItem.fromJson(<String, dynamic>{
        'id': 'x',
        'allowedActions': 'oops',
        'logs': 42,
      });
      expect(item.id, 'x');
      expect(item.allowedActions, isEmpty);
      final d = FeedbackDetail.fromJson(<String, dynamic>{});
      expect(d.id, '');
      expect(d.displayTitle, '（无标题）');
    });

    test('操作结果：responseOk 携带 detail 快照', () {
      final actionJson = _fixture('feedback-action.json');
      final result = FeedbackActionResult.fromJson(
          asMapFixture(actionJson['responseOk']));
      expect(result.ok, true);
      expect(result.action, 'archive');
      expect(result.replayed, false);
      expect(result.detail, isNotNull);
      expect(result.detail!.mgmtState, 'archived');
      expect(result.detail!.lifecycleVersion, 1);
      expect(result.detail!.allowedActions, ['unarchive', 'trash']);
    });

    test('action 枚举：生命周期动作与 revision 动作区分', () {
      expect(FeedbackAction.archive.isLifecycle, true);
      expect(FeedbackAction.resumeProcessing.isLifecycle, true);
      expect(FeedbackAction.retry.isLifecycle, false);
      expect(FeedbackAction.retry.needsRevision, true);
      expect(FeedbackAction.recheck.needsRevision, true);
      expect(FeedbackAction.archive.needsRevision, false);
      expect(FeedbackAction.fromWire('unknown'), isNull);
      expect(FeedbackAction.fromWire('trash'), FeedbackAction.trash);
      expect(FeedbackView.fromWire('trash'), FeedbackView.trash);
      expect(FeedbackView.fromWire('bogus'), FeedbackView.inbox);
    });
  });

  group('requestId（ULID 幂等键）', () {
    final pattern = RegExp(r'^rop_[0-9A-HJKMNP-TV-Z]{26}$');

    test('格式符合契约 [A-Za-z0-9._:-]{1,128}，rop_ + 26 位 Crockford', () {
      for (var i = 0; i < 200; i++) {
        final id = generateFeedbackRequestId();
        expect(id, matches(pattern));
        expect(id.length, 30);
      }
    });

    test('每次生成唯一（不重复）', () {
      final seen = <String>{};
      for (var i = 0; i < 500; i++) {
        expect(seen.add(generateFeedbackRequestId()), true);
      }
    });
  });

  group('ApiException 反馈错误分类', () {
    ApiException err(String code, {String? detail}) => ApiException.fromBody(
          409,
          jsonEncode({
            'error': {'code': code, 'message': 'm-$code'},
            if (detail != null) 'detail': jsonDecode(detail),
          }),
        );

    test('分类映射：retryable / needsUpgrade / needsConfig / uncertain / conflict', () {
      expect(err('feedback_unavailable').feedbackCategory,
          FeedbackErrorCategory.retryable);
      expect(err('busy').feedbackCategory,
          FeedbackErrorCategory.retryable);
      expect(err('rate_limited').feedbackCategory,
          FeedbackErrorCategory.retryable);
      expect(err('feedback_mgmt_unsupported').feedbackCategory,
          FeedbackErrorCategory.needsUpgrade);
      expect(err('feedback_mgmt_not_configured').feedbackCategory,
          FeedbackErrorCategory.needsConfig);
      expect(err('feedback_upstream_not_configured').feedbackCategory,
          FeedbackErrorCategory.needsConfig);
      expect(err('outcome_uncertain').feedbackCategory,
          FeedbackErrorCategory.uncertain);
      expect(err('request_in_flight').feedbackCategory,
          FeedbackErrorCategory.uncertain);
      expect(err('version_conflict').feedbackCategory,
          FeedbackErrorCategory.conflict);
      expect(err('revision_conflict').feedbackCategory,
          FeedbackErrorCategory.conflict);
      expect(err('invalid_state').feedbackCategory,
          FeedbackErrorCategory.conflict);
      expect(err('not_found').feedbackCategory, FeedbackErrorCategory.other);
      expect(err('request_id_conflict').feedbackCategory,
          FeedbackErrorCategory.other);
      expect(err('internal').feedbackCategory, FeedbackErrorCategory.other);
    });

    test('错误码辅助 getter', () {
      expect(err('feedback_unavailable').isFeedbackUnavailable, true);
      expect(err('feedback_mgmt_not_configured').isFeedbackMgmtNotConfigured,
          true);
      expect(err('feedback_upstream_not_configured')
          .isFeedbackUpstreamNotConfigured, true);
      expect(
          err('feedback_mgmt_unsupported').isFeedbackMgmtUnsupported, true);
      expect(err('revision_conflict').isRevisionConflict, true);
      expect(err('invalid_state').isInvalidState, true);
      expect(err('busy').isBusy, true);
      expect(err('request_id_conflict').isRequestIdConflict, true);
      expect(err('request_in_flight').isRequestInFlight, true);
      expect(err('outcome_uncertain').isOutcomeUncertain, true);
    });

    test('version_conflict 响应携带 detail 快照（fixture）', () {
      final fixture = _fixture('feedback-action.json');
      final conflict = asMapFixture(fixture['responseVersionConflict']);
      final e = ApiException.fromBody(409, jsonEncode(conflict));
      expect(e.code, 'version_conflict');
      expect(e.isVersionConflict, true);
      expect(e.feedbackCategory, FeedbackErrorCategory.conflict);
      expect(e.detail, isNotNull);
      // detail 快照可直接回灌 FeedbackDetail（就地刷新路径）。
      final d = FeedbackDetail.fromJson(e.detail!);
      expect(d.lifecycleVersion, 1);
      expect(d.mgmtState, 'archived');
      expect(d.allowedActions, ['unarchive', 'trash']);
    });

    test('非 JSON / 空体兜底', () {
      final e = ApiException.fromBody(500, 'oops');
      expect(e.code, 'internal');
      final e2 = ApiException.fromBody(404, '');
      expect(e2.code, 'not_found');
      expect(e2.isNotFound, true);
    });
  });

  group('HttpAssistantApi 反馈端点（MockClient）', () {
    late List<http.BaseRequest> requests;

    HttpAssistantApi api(String Function(http.Request req) handler) {
      requests = <http.BaseRequest>[];
      final client = http_testing.MockClient((req) async {
        requests.add(req);
        return http.Response(handler(req), 200,
            headers: {'content-type': 'application/json'});
      });
      final api = HttpAssistantApi(client: client);
      api.bindSession(_session());
      return api;
    }

    test('列表：GET 路径 + view/cursor/limit/q 查询参数', () async {
      final a = api((req) {
        expect(req.method, 'GET');
        return jsonEncode(_fixture('feedback-list.json'));
      });
      final page = await a.getFeedbackList(
        sourceId: 'src 1',
        view: 'archived',
        cursor: 'cur/1',
        limit: 30,
        q: '趋势图',
      );
      final uri = requests.single.url;
      expect(uri.path, '/api/v1/sources/src%201/feedback');
      expect(uri.queryParameters['view'], 'archived');
      expect(uri.queryParameters['cursor'], 'cur/1');
      expect(uri.queryParameters['limit'], '30');
      expect(uri.queryParameters['q'], '趋势图');
      expect(page.items, hasLength(2));
      expect(page.counts['archived'], 12);
      expect(page.nextCursor, isNotNull);
    });

    test('列表：q 缺省时不携带 q 参数', () async {
      final a = api((req) => jsonEncode(
          {'items': <dynamic>[], 'counts': <String, int>{}}));
      await a.getFeedbackList(sourceId: 's1');
      final uri = requests.single.url;
      expect(uri.queryParameters.containsKey('q'), false);
      expect(uri.queryParameters['view'], 'inbox');
      expect(uri.queryParameters.containsKey('cursor'), false);
    });

    test('详情：GET /sources/{src}/feedback/{fb}', () async {
      final a = api((req) => jsonEncode(_fixture('feedback-detail.json')));
      final d = await a.getFeedbackDetail(
          sourceId: 'src_01', feedbackId: 'fb/9');
      final uri = requests.single.url;
      expect(uri.path, '/api/v1/sources/src_01/feedback/fb%2F9');
      expect(d.id, '0198f3a2-7c4e-7b2d-9e5f-1a2b3c4d5e6f');
      expect(d.manage, true);
    });

    test('生命周期动作：body = requestId + action + expectedLifecycleVersion', () async {
      final a = api((req) {
        expect(req.method, 'POST');
        return jsonEncode(asMapFixture(
            _fixture('feedback-action.json')['responseOk']));
      });
      final result = await a.postFeedbackAction(
        sourceId: 'src1',
        feedbackId: 'fb1',
        requestId: 'rop_01J8WF2E0X4M7K8Q2V9B1C3D4E',
        action: 'archive',
        expectedLifecycleVersion: 0,
      );
      final req = requests.single as http.Request;
      expect(req.url.path,
          '/api/v1/sources/src1/feedback/fb1/action');
      final body = asMapFixture(jsonDecode(req.body));
      expect(body, {
        'requestId': 'rop_01J8WF2E0X4M7K8Q2V9B1C3D4E',
        'action': 'archive',
        'expectedLifecycleVersion': 0,
      });
      // 未传 expectedRevision → 请求体不得携带该键。
      expect(body.containsKey('expectedRevision'), false);
      expect(result.replayed, false);
      expect(result.detail!.mgmtState, 'archived');
    });

    test('retry/recheck 类动作：body 携带 expectedRevision', () async {
      final a = api((req) => jsonEncode(
          {'ok': true, 'action': 'retry', 'replayed': false}));
      await a.postFeedbackAction(
        sourceId: 's',
        feedbackId: 'f',
        requestId: 'r1',
        action: 'retry',
        expectedRevision: 7,
      );
      final body =
          asMapFixture(jsonDecode((requests.single as http.Request).body));
      expect(body, {
        'requestId': 'r1',
        'action': 'retry',
        'expectedRevision': 7,
      });
      expect(body.containsKey('expectedLifecycleVersion'), false);
    });

    test('幂等：同 requestId 重复调用产生逐字节相同的请求体（安全重发语义）',
        () async {
      final bodies = <String>[];
      final client = http_testing.MockClient((req) async {
        bodies.add(req.body);
        // 服务端幂等回放：replayed=true。
        return http.Response(
            jsonEncode(
                {'ok': true, 'action': 'archive', 'replayed': true}),
            200,
            headers: {'content-type': 'application/json'});
      });
      final a = HttpAssistantApi(client: client)..bindSession(_session());
      const requestId = 'rop_FIXED01J8WF2E0X4M7K8Q2V9B1C3';
      for (var i = 0; i < 2; i++) {
        final r = await a.postFeedbackAction(
          sourceId: 's',
          feedbackId: 'f',
          requestId: requestId,
          action: 'archive',
          expectedLifecycleVersion: 3,
        );
        expect(r.replayed, true);
      }
      expect(bodies, hasLength(2));
      expect(bodies[0], bodies[1]);
      expect(jsonDecode(bodies[0])['requestId'], requestId);
    });

    test('附件：GET 路径通配 + Bearer + 字节/类型回传', () async {
      final client = http_testing.MockClient((req) async {
        return http.Response.bytes(
            const [1, 2, 3], 200,
            headers: {'content-type': 'text/plain; charset=utf-8'});
      });
      final a = HttpAssistantApi(client: client)..bindSession(_session());
      final res = await a.fetchFeedbackAttachment(
          's1', 'f1', 'logs/log id');
      expect(res.bytes, [1, 2, 3]);
      expect(res.contentType, contains('text/plain'));
    });

    test('attachmentUri 逐段编码保留 logs/ 路径结构', () {
      final a = HttpAssistantApi()..bindSession(_session());
      final uri = a.feedbackAttachmentUri('s 1', 'f1', 'logs/l 2');
      expect(uri.path,
          '/api/v1/sources/s%201/feedback/f1/attachments/logs/l%202');
    });

    test('错误响应：信封解析为 ApiException', () async {
      final client = http_testing.MockClient((req) async {
        return http.Response(
            jsonEncode({
              'error': {
                'code': 'feedback_mgmt_not_configured',
                'message': '该来源未配置管理凭证'
              }
            }),
            409,
            headers: {'content-type': 'application/json'});
      });
      final a = HttpAssistantApi(client: client)..bindSession(_session());
      expect(
        () => a.getFeedbackList(sourceId: 's'),
        throwsA(isA<ApiException>()
            .having((e) => e.code, 'code', 'feedback_mgmt_not_configured')
            .having((e) => e.feedbackCategory, 'category',
                FeedbackErrorCategory.needsConfig)),
      );
    });
  });

  group('runAction 幂等 / 重试语义（ProviderContainer）', () {
    const target = (sourceId: 's', feedbackId: 'f');

    test('uncertain 后显式重试同一动作复用 requestId；成功后新动作用新值',
        () async {
      final fake = _FakeApi()
        ..detail =
            FeedbackDetail.fromJson(_fixture('feedback-detail.json'));
      final container = ProviderContainer(overrides: [
        apiProvider.overrideWithValue(fake),
        connectivityMonitorProvider.overrideWithValue(_FakeConnectivity()),
      ]);
      addTearDown(container.dispose);
      // 保持 autoDispose family 实例存活。
      final sub =
          container.listen(feedbackDetailProvider(target), (_, _) {});
      addTearDown(sub.close);
      await container.read(feedbackDetailProvider(target).future);

      // ① 网络错误 → uncertain，requestId 留存。
      fake.actionError = const ApiNetworkException('timeout');
      final notifier =
          container.read(feedbackDetailProvider(target).notifier);
      final o1 = await notifier.runAction(FeedbackAction.archive);
      expect(o1.status, FeedbackActionStatus.uncertain);
      expect(fake.actionCalls, hasLength(1));
      final rid1 = fake.actionCalls.single['requestId'] as String;
      expect(rid1, matches(RegExp(r'^rop_[0-9A-Z]{26}$')));

      // ② 用户显式重试同一动作 → 复用同一 requestId（服务端幂等回放安全）。
      fake.actionError = null;
      fake.actionResult = FeedbackActionResult.fromJson(asMapFixture(
          _fixture('feedback-action.json')['responseOk']));
      final o2 = await notifier.runAction(FeedbackAction.archive);
      expect(o2.status, FeedbackActionStatus.ok);
      expect(fake.actionCalls, hasLength(2));
      expect(fake.actionCalls[1]['requestId'], rid1);

      // ③ 已确定成功后，下一次动作生成全新 requestId。
      await notifier.runAction(FeedbackAction.trash);
      expect(fake.actionCalls, hasLength(3));
      expect(fake.actionCalls[2]['requestId'], isNot(rid1));
    });

    test('不同动作不复用 pending requestId', () async {
      final fake = _FakeApi()
        ..detail =
            FeedbackDetail.fromJson(_fixture('feedback-detail.json'));
      final container = ProviderContainer(overrides: [
        apiProvider.overrideWithValue(fake),
        connectivityMonitorProvider.overrideWithValue(_FakeConnectivity()),
      ]);
      addTearDown(container.dispose);
      final sub =
          container.listen(feedbackDetailProvider(target), (_, _) {});
      addTearDown(sub.close);
      await container.read(feedbackDetailProvider(target).future);
      final notifier =
          container.read(feedbackDetailProvider(target).notifier);

      fake.actionError = const ApiNetworkException('timeout');
      await notifier.runAction(FeedbackAction.archive);
      final ridArchive = fake.actionCalls.single['requestId'];

      // 换成 trash → 属新动作，必须新 requestId。
      await notifier.runAction(FeedbackAction.trash);
      expect(fake.actionCalls[1]['requestId'], isNot(ridArchive));
    });

    test('冲突响应的 detail 快照就地刷新详情', () async {
      final conflict = asMapFixture(
          _fixture('feedback-action.json')['responseVersionConflict']);
      final fake = _FakeApi()
        ..detail =
            FeedbackDetail.fromJson(_fixture('feedback-detail.json'))
        ..actionError = ApiException.fromBody(409, jsonEncode(conflict));
      final container = ProviderContainer(overrides: [
        apiProvider.overrideWithValue(fake),
        connectivityMonitorProvider.overrideWithValue(_FakeConnectivity()),
      ]);
      addTearDown(container.dispose);
      final sub =
          container.listen(feedbackDetailProvider(target), (_, _) {});
      addTearDown(sub.close);
      await container.read(feedbackDetailProvider(target).future);
      final notifier =
          container.read(feedbackDetailProvider(target).notifier);

      final outcome = await notifier.runAction(FeedbackAction.archive);
      expect(outcome.status, FeedbackActionStatus.conflict);
      final d = container.read(feedbackDetailProvider(target)).value;
      expect(d!.lifecycleVersion, 1);
      expect(d.mgmtState, 'archived');
    });
  });

  // ---------------- widget 冒烟（mocked providers） ----------------

  group('反馈管理页面冒烟', () {
    testWidgets('列表：三区页签 + counts + 条目渲染 + 点按导航', (tester) async {
      final fake = _FakeApi()
        ..sources = [
          const Source(id: 'src1', name: 'Feedback', kind: 'feedback'),
        ]
        ..listResult = FeedbackListResult(
          items: (_fixture('feedback-list.json')['items'] as List)
              .map((e) => FeedbackItem.fromJson(asMapFixture(e)))
              .toList(),
          nextCursor: null,
          counts: const {'inbox': 2, 'archived': 12, 'trash': 1},
        );
      await tester.pumpWidget(_wrap(
        const FeedbackListPage(),
        api: fake,
        extraRoutes: [
          GoRoute(
            path: '/sources/:srcId/feedback/:fbId',
            builder: (_, state) => Scaffold(
                body: Text('detail:${state.pathParameters['fbId']}')),
          ),
        ],
      ));
      await tester.pumpAndSettle();
      expect(find.text('反馈管理'), findsOneWidget);
      expect(find.text('收件箱 2'), findsOneWidget);
      expect(find.text('已归档 12'), findsOneWidget);
      expect(find.text('回收站 1'), findsOneWidget);
      expect(find.text('首页趋势图偶发不刷新'), findsOneWidget);
      // 第二条 title 缺省 → 预览文本作为标题。
      expect(find.text('通知点了没反应'), findsOneWidget);
      // 错误摘要徽标。
      expect(find.textContaining('Kaneo 附件'), findsOneWidget);

      await tester.tap(find.text('首页趋势图偶发不刷新'));
      await tester.pumpAndSettle();
      expect(
          find.text('detail:0198f3a2-7c4e-7b2d-9e5f-1a2b3c4d5e6f'),
          findsOneWidget);
    });

    testWidgets('列表：未配置管理凭证 → 引导视图', (tester) async {
      final fake = _FakeApi()
        ..sources = [
          const Source(id: 'src1', name: 'Feedback', kind: 'feedback'),
        ]
        ..listError = const ApiException(
            code: 'feedback_mgmt_not_configured',
            message: '该来源未配置管理凭证');
      await tester.pumpWidget(_wrap(
        const FeedbackListPage(),
        api: fake,
        extraRoutes: [
          GoRoute(
              path: '/sources',
              builder: (_, _) =>
                  const Scaffold(body: Text('sources-page'))),
        ],
      ));
      await tester.pumpAndSettle();
      expect(find.text('该来源未配置管理凭证'), findsOneWidget);
      expect(find.text('去接入管理'), findsOneWidget);
      await tester.tap(find.text('去接入管理'));
      await tester.pumpAndSettle();
      expect(find.text('sources-page'), findsOneWidget);
    });

    testWidgets('列表：需升级提示（旧版服务端）', (tester) async {
      final fake = _FakeApi()
        ..sources = [
          const Source(id: 'src1', name: 'Feedback', kind: 'feedback'),
        ]
        ..listError = const ApiException(
            code: 'feedback_mgmt_unsupported',
            message: 'Feedback 服务不支持管理接口');
      await tester.pumpWidget(_wrap(const FeedbackListPage(), api: fake));
      await tester.pumpAndSettle();
      expect(find.text('需升级 Feedback 服务'), findsOneWidget);
    });

    testWidgets('详情：渲染 + 归档动作成功 → 响应 detail 就地刷新', (tester) async {
      final fake = _FakeApi()
        ..detail =
            FeedbackDetail.fromJson(_fixture('feedback-detail.json'))
        ..actionResult = FeedbackActionResult.fromJson(asMapFixture(
            _fixture('feedback-action.json')['responseOk']));
      await tester.pumpWidget(_wrap(
        const FeedbackDetailPage(
          sourceId: 'src_01J8WEXAMPLE000000000000',
          feedbackId: '0198f3a2-7c4e-7b2d-9e5f-1a2b3c4d5e6f',
        ),
        api: fake,
      ));
      await tester.pumpAndSettle();
      expect(find.text('首页趋势图偶发不刷新'), findsOneWidget);
      // 生命周期版本行（值是 SelectableText → findRichText）。
      expect(find.text('生命周期版本'), findsOneWidget);
      expect(find.text('v0', findRichText: true), findsOneWidget);
      // allowedActions=[archive,trash] → 两个动作按钮（在懒加载列表底部，先滚出）。
      await _scrollToText(tester, '归档');
      expect(find.text('归档'), findsOneWidget);
      expect(find.text('移入回收站'), findsOneWidget);

      await tester.tap(find.text('归档'));
      await tester.pumpAndSettle();
      // 动作请求：requestId 符合格式、携带 expectedLifecycleVersion=0。
      expect(fake.actionCalls, hasLength(1));
      expect(fake.actionCalls.single['action'], 'archive');
      expect(fake.actionCalls.single['expectedLifecycleVersion'], 0);
      expect(
          fake.actionCalls.single['requestId'] as String,
          matches(RegExp(r'^rop_[0-9A-Z]{26}$')));
      // 成功提示 + detail 已切到 archived。
      expect(find.text('操作已执行'), findsOneWidget);
      // 刷新后 allowedActions 变为 [unarchive, trash]。
      expect(find.text('取消归档'), findsOneWidget);
    });

    testWidgets('详情：trash 需二次确认，取消不发请求', (tester) async {
      final fake = _FakeApi()
        ..detail =
            FeedbackDetail.fromJson(_fixture('feedback-detail.json'));
      await tester.pumpWidget(_wrap(
        const FeedbackDetailPage(sourceId: 's', feedbackId: 'f'),
        api: fake,
      ));
      await tester.pumpAndSettle();
      await _scrollToText(tester, '移入回收站');
      await tester.tap(find.text('移入回收站'));
      await tester.pumpAndSettle();
      expect(
          find.text('移入回收站后可在「回收站」视图恢复。确定继续？'),
          findsOneWidget);
      await tester.tap(find.descendant(
          of: find.byType(AlertDialog), matching: find.text('取消')));
      await tester.pumpAndSettle();
      expect(fake.actionCalls, isEmpty);
      // 再次点击 → 确认 → 发请求。
      await _scrollToText(tester, '移入回收站');
      await tester.tap(find.text('移入回收站'));
      await tester.pumpAndSettle();
      await tester.tap(find.descendant(
          of: find.byType(AlertDialog),
          matching: find.text('移入回收站')));
      await tester.pumpAndSettle();
      expect(fake.actionCalls, hasLength(1));
      expect(fake.actionCalls.single['action'], 'trash');
    });

    testWidgets('详情：版本冲突 → 用 detail 快照刷新并提示', (tester) async {
      final conflict = asMapFixture(
          _fixture('feedback-action.json')['responseVersionConflict']);
      final fake = _FakeApi()
        ..detail =
            FeedbackDetail.fromJson(_fixture('feedback-detail.json'))
        ..actionError = ApiException.fromBody(409, jsonEncode(conflict));
      await tester.pumpWidget(_wrap(
        const FeedbackDetailPage(sourceId: 's', feedbackId: 'f'),
        api: fake,
      ));
      await tester.pumpAndSettle();
      await _scrollToText(tester, '归档');
      await tester.tap(find.text('归档'));
      await tester.pumpAndSettle();
      expect(find.textContaining('已刷新为最新状态'), findsOneWidget);
      // 快照 allowedActions=[unarchive,trash] → 「取消归档」按钮出现。
      expect(find.text('取消归档'), findsOneWidget);
    });

    testWidgets('详情：网络错误 → 「结果待确认」+ 自动拉详情核对，不重发',
        (tester) async {
      var detailFetches = 0;
      final fake = _FakeApi()
        ..detail =
            FeedbackDetail.fromJson(_fixture('feedback-detail.json'))
        ..actionError = const ApiNetworkException('timeout')
        ..onDetailFetch = () => detailFetches++;
      await tester.pumpWidget(_wrap(
        const FeedbackDetailPage(sourceId: 's', feedbackId: 'f'),
        api: fake,
      ));
      await tester.pumpAndSettle();
      expect(detailFetches, 1);
      await _scrollToText(tester, '归档');
      await tester.tap(find.text('归档'));
      await tester.pumpAndSettle();
      expect(find.textContaining('结果待确认'), findsOneWidget);
      // 动作只发了一次 + 详情被重新拉取核对。
      expect(fake.actionCalls, hasLength(1));
      expect(detailFetches, 2);
    });
  });
}

/// 列表懒构建：滚动直到目标文本进入视口（供操作按钮等页底元素）。
Future<void> _scrollToText(WidgetTester tester, String text) {
  return tester.scrollUntilVisible(
    find.text(text),
    300,
    scrollable: find.byType(Scrollable).first,
  );
}

// ---------------- 测试替身 ----------------

/// 固定在线的连接监视器（避免测试环境触发 connectivity 插件）。
class _FakeConnectivity implements ConnectivityMonitor {
  @override
  Future<bool> isOnline() async => true;

  @override
  Stream<bool> get onlineStream => const Stream<bool>.empty();
}

Widget _wrap(
  Widget child, {
  required _FakeApi api,
  List<RouteBase> extraRoutes = const [],
}) {
  final router = GoRouter(
    routes: [
      GoRoute(path: '/', builder: (_, _) => child),
      ...extraRoutes,
    ],
  );
  return ProviderScope(
    overrides: [
      apiProvider.overrideWithValue(api),
      connectivityMonitorProvider.overrideWithValue(_FakeConnectivity()),
    ],
    child: MaterialApp.router(routerConfig: router),
  );
}

/// 反馈管理测试用 Fake：只实现被调用到的方法，其余抛 UnimplementedError。
class _FakeApi implements AssistantApi {
  List<Source> sources = const [];
  FeedbackListResult? listResult;
  Object? listError;
  FeedbackDetail? detail;
  FeedbackActionResult? actionResult;
  Object? actionError;
  void Function()? onDetailFetch;

  /// postFeedbackAction 收到的参数（按调用顺序）。
  final List<Map<String, Object?>> actionCalls = [];

  @override
  Session? get session => _session;
  Session? _session;

  @override
  void bindSession(Session? session) => _session = session;

  @override
  Future<List<Source>> getSources() async => sources;

  @override
  Future<FeedbackListResult> getFeedbackList({
    required String sourceId,
    String view = 'inbox',
    String? cursor,
    int limit = 50,
    String? q,
  }) async {
    final e = listError;
    if (e != null) throw e;
    return listResult ??
        const FeedbackListResult(items: [], counts: {});
  }

  @override
  Future<FeedbackDetail> getFeedbackDetail({
    required String sourceId,
    required String feedbackId,
  }) async {
    onDetailFetch?.call();
    return detail ??
        FeedbackDetail.fromJson(const <String, dynamic>{'id': 'f'});
  }

  @override
  Future<FeedbackActionResult> postFeedbackAction({
    required String sourceId,
    required String feedbackId,
    required String requestId,
    required String action,
    int? expectedLifecycleVersion,
    int? expectedRevision,
  }) async {
    actionCalls.add({
      'requestId': requestId,
      'action': action,
      'expectedLifecycleVersion': expectedLifecycleVersion,
      'expectedRevision': expectedRevision,
    });
    final e = actionError;
    if (e != null) throw e;
    return actionResult ??
        FeedbackActionResult(ok: true, action: action, detail: detail);
  }

  @override
  Uri feedbackAttachmentUri(
          String sourceId, String feedbackId, String attachmentId) =>
      Uri.parse(
          'http://hub.test/api/v1/sources/$sourceId/feedback/$feedbackId/attachments/$attachmentId');

  @override
  Future<AttachmentBytes> fetchFeedbackAttachment(
          String sourceId, String feedbackId, String attachmentId) =>
      throw UnimplementedError();

  // ---- 其余接口（本组测试不触达） ----

  @override
  Uri attachmentUri(String messageId, String attachmentId) =>
      throw UnimplementedError();

  @override
  Future<CreatedSource> createSource(
          {required String name,
          required String kind,
          String? attachmentBaseUrl}) =>
      throw UnimplementedError();

  @override
  Future<void> deleteSource(String id) => throw UnimplementedError();

  @override
  Future<AttachmentBytes> fetchAttachment(
          String messageId, String attachmentId) =>
      throw UnimplementedError();

  @override
  Future<FaultPage> getFaults(
          {String state = 'open', String? cursor, int limit = 50}) =>
      throw UnimplementedError();

  @override
  Future<MessageDetail> getMessage(String id) => throw UnimplementedError();

  @override
  Future<MessagePage> getMessages(
          {String? cursor,
          int limit = 50,
          MessageFilter filter = MessageFilter.all,
          String? source}) =>
      throw UnimplementedError();

  @override
  Future<List<MetricPoint>> getMetricSeries(
          {required String source,
          required String metric,
          required DateTime from,
          required DateTime to,
          String step = 'raw',
          String? label}) =>
      throw UnimplementedError();

  @override
  Future<Overview> getOverview() => throw UnimplementedError();

  @override
  Future<Rules> getRules() => throw UnimplementedError();

  @override
  Future<Settings> getSettings() => throw UnimplementedError();

  @override
  Future<SyncPage> getSync({int since = 0, int limit = 200}) =>
      throw UnimplementedError();

  @override
  Future<LoginResult> login(
          {required String hubUrl,
          required String username,
          required String password,
          String? deviceLabel}) =>
      throw UnimplementedError();

  @override
  Future<void> logout() => throw UnimplementedError();

  @override
  Future<int> markAllRead({DateTime? before}) => throw UnimplementedError();

  @override
  Future<Fault> markFaultRead(String id) => throw UnimplementedError();

  @override
  Future<Message> markMessageRead(String id) => throw UnimplementedError();

  @override
  Future<Message> markMessageUnread(String id) => throw UnimplementedError();

  @override
  Future<Fault> muteFault(String id) => throw UnimplementedError();

  @override
  Future<Settings> patchSettings(
          {required int expectedVersion,
          DndSettings? dnd,
          int? reportIntervalSeconds}) =>
      throw UnimplementedError();

  @override
  Future<Rules> putRules(
          {required int expectedVersion,
          int? heartbeatSeconds,
          required List<AlertRule> rules}) =>
      throw UnimplementedError();

  @override
  Future<LoginResult> refreshSession() => throw UnimplementedError();

  @override
  Future<RotatedKey> rotateSourceKey(String id) => throw UnimplementedError();

  @override
  Future<Fault> unmuteFault(String id) => throw UnimplementedError();

  @override
  Future<Source> updateSource(String id,
          {String? name,
          bool? enabled,
          String? attachmentBaseUrl,
          bool clearAttachmentBaseUrl = false}) =>
      throw UnimplementedError();
}
