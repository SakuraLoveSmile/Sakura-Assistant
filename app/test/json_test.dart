import 'package:assistant/models/json.dart';
import 'package:assistant/models/message.dart';
import 'package:assistant/models/session.dart';
import 'package:assistant/state/session.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  group('json helpers', () {
    test('asMap 容忍非 Map 值', () {
      expect(asMap(null), isEmpty);
      expect(asMap('x'), isEmpty);
      expect(asMap(<String, dynamic>{'a': 1})['a'], 1);
      expect(asMap(<int, String>{1: 'v'})['1'], 'v');
    });

    test('asInt 解析 num / 字符串 / 回退', () {
      expect(asInt(7), 7);
      expect(asInt(7.9), 7);
      expect(asInt('42'), 42);
      expect(asInt('x', -1), -1);
      expect(asInt(null, 3), 3);
    });

    test('asBool 解析 bool / num / 字符串', () {
      expect(asBool(true), true);
      expect(asBool(1), true);
      expect(asBool(0), false);
      expect(asBool('true'), true);
      expect(asBool('FALSE'), false);
      expect(asBool('x', true), true);
    });

    test('asDateOrNull 解析 RFC3339 与容错', () {
      expect(asDateOrNull('2026-09-18T06:30:00.000Z')!.isUtc, true);
      expect(asDateOrNull('bad'), isNull);
      expect(asDateOrNull(123), isNull);
      expect(asDateOrNull(''), isNull);
    });
  });

  group('normalizeHubUrl', () {
    test('补默认 scheme：IP/localhost → http，域名 → https', () {
      expect(normalizeHubUrl('192.168.1.10:8795'), 'http://192.168.1.10:8795');
      expect(normalizeHubUrl('localhost:8795'), 'http://localhost:8795');
      expect(normalizeHubUrl('nas.local'), 'http://nas.local');
      expect(normalizeHubUrl('hub.example.com'), 'https://hub.example.com');
    });

    test('已有 scheme 不重复补；去尾斜杠与空格', () {
      expect(normalizeHubUrl('http://a.b/'), 'http://a.b');
      expect(normalizeHubUrl('  https://a.b///  '), 'https://a.b');
      expect(normalizeHubUrl(''), '');
    });
  });

  group('Session', () {
    test('toJson/fromJson 往返一致', () {
      final s = Session(
        hubUrl: 'http://h:8795',
        token: 'tok',
        refreshToken: 'ref',
        username: 'u',
        expiresAt: DateTime.utc(2026, 9, 18, 6, 30),
      );
      final back = Session.fromJson(s.toJson());
      expect(back.hubUrl, s.hubUrl);
      expect(back.token, s.token);
      expect(back.refreshToken, s.refreshToken);
      expect(back.username, s.username);
      expect(back.expiresAt, s.expiresAt);
    });

    test('copyWith 只覆盖给定字段（凭证旋转场景）', () {
      final s = Session(
        hubUrl: 'http://h:8795',
        token: 't1',
        refreshToken: 'r1',
        username: 'u',
      );
      final next = s.copyWith(token: 't2', refreshToken: 'r2');
      expect(next.token, 't2');
      expect(next.refreshToken, 'r2');
      expect(next.hubUrl, s.hubUrl);
      expect(next.username, s.username);
    });
  });

  group('Message.fromJson', () {
    test('缺失字段回退默认，未知字段忽略（契约容错）', () {
      final m = Message.fromJson(<String, dynamic>{
        'id': 'm1',
        'title': 't',
        'unexpected': {'nested': true},
      });
      expect(m.id, 'm1');
      expect(m.severity, 'info');
      expect(m.isRead, false);
      expect(m.belongsToFault, false);
    });
  });
}
