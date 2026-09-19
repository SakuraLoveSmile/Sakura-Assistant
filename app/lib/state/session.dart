import 'dart:convert';

import 'package:flutter_secure_storage/flutter_secure_storage.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../models/json.dart';
import '../models/session.dart';

/// 会话持久化抽象：token/refreshToken 走安全存储，hubUrl 走偏好。
/// widget 测试用内存实现替换。
abstract class TokenStore {
  Future<Session?> load();

  Future<void> save(Session session);

  Future<void> clear();
}

/// flutter_secure_storage（Keystore）实现。
class SecureTokenStore implements TokenStore {
  SecureTokenStore(this._prefs, [FlutterSecureStorage? storage])
      : _storage = storage ?? const FlutterSecureStorage();

  final SharedPreferences _prefs;
  final FlutterSecureStorage _storage;

  static const _kToken = 'auth.token';
  static const _kRefresh = 'auth.refreshToken';
  static const _kUsername = 'auth.username';
  static const _kExpiresAt = 'auth.expiresAt';
  static const _kHubUrl = 'hub.url'; // 非敏感，放偏好

  @override
  Future<Session?> load() async {
    try {
      final token = await _storage.read(key: _kToken);
      final refresh = await _storage.read(key: _kRefresh);
      final hubUrl = _prefs.getString(_kHubUrl);
      if (token == null || token.isEmpty || refresh == null || hubUrl == null) {
        return null;
      }
      return Session(
        hubUrl: hubUrl,
        token: token,
        refreshToken: refresh,
        username: await _storage.read(key: _kUsername) ?? '',
        expiresAt: asDateOrNull(await _storage.read(key: _kExpiresAt)),
      );
    } catch (_) {
      // 安全存储不可用时视为未登录，不崩溃。
      return null;
    }
  }

  @override
  Future<void> save(Session session) async {
    await _prefs.setString(_kHubUrl, session.hubUrl);
    await _storage.write(key: _kToken, value: session.token);
    await _storage.write(key: _kRefresh, value: session.refreshToken);
    await _storage.write(key: _kUsername, value: session.username);
    await _storage.write(
        key: _kExpiresAt,
        value: session.expiresAt?.toUtc().toIso8601String() ?? '');
  }

  @override
  Future<void> clear() async {
    await _storage.delete(key: _kToken);
    await _storage.delete(key: _kRefresh);
    await _storage.delete(key: _kUsername);
    await _storage.delete(key: _kExpiresAt);
  }

  /// 只读 hubUrl（未登录时预填登录框）。
  String? savedHubUrl() => _prefs.getString(_kHubUrl);
}

/// 内存实现（测试用）。
class MemoryTokenStore implements TokenStore {
  Session? _session;
  String? hubUrl;

  @override
  Future<Session?> load() async => _session;

  @override
  Future<void> save(Session session) async {
    _session = session;
    hubUrl = session.hubUrl;
  }

  @override
  Future<void> clear() async => _session = null;

  String? savedHubUrl() => hubUrl;
}

/// 规范化用户输入的 hub 地址：去空格/尾斜杠，补默认 scheme
///（IP/localhost 默认 http，域名默认 https）。
String normalizeHubUrl(String input) {
  var url = input.trim();
  if (url.isEmpty) return url;
  if (!url.contains('://')) {
    final host = url.split('/').first.split(':').first;
    final isIp = RegExp(r'^\d{1,3}(\.\d{1,3}){3}$').hasMatch(host) ||
        host == 'localhost' ||
        host.endsWith('.local') ||
        host.endsWith('.lan');
    url = '${isIp ? 'http' : 'https'}://$url';
  }
  while (url.endsWith('/')) {
    url = url.substring(0, url.length - 1);
  }
  return url;
}

/// 把 Session 序列化为 map 备用（调试用）。
Map<String, dynamic> sessionToMap(Session s) => s.toJson();

Session? sessionFromJsonString(String? raw) =>
    raw == null ? null : Session.fromJson(asMap(jsonDecode(raw)));
