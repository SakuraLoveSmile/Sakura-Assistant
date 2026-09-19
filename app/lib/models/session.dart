import 'json.dart';

/// 登录会话：客户端令牌 + 中枢地址 + 账号。
/// token/refreshToken 持久化在 flutter_secure_storage，hubUrl 在 SharedPreferences。
class Session {
  const Session({
    required this.hubUrl,
    required this.token,
    required this.refreshToken,
    required this.username,
    this.expiresAt,
  });

  final String hubUrl;
  final String token;
  final String refreshToken;
  final String username;
  final DateTime? expiresAt;

  factory Session.fromJson(Map<String, dynamic> json) => Session(
        hubUrl: asString(json['hubUrl']),
        token: asString(json['token']),
        refreshToken: asString(json['refreshToken']),
        username: asString(json['username']),
        expiresAt: asDateOrNull(json['expiresAt']),
      );

  Map<String, dynamic> toJson() => {
        'hubUrl': hubUrl,
        'token': token,
        'refreshToken': refreshToken,
        'username': username,
        'expiresAt': expiresAt?.toUtc().toIso8601String(),
      };

  Session copyWith({String? token, String? refreshToken, DateTime? expiresAt}) =>
      Session(
        hubUrl: hubUrl,
        token: token ?? this.token,
        refreshToken: refreshToken ?? this.refreshToken,
        username: username,
        expiresAt: expiresAt ?? this.expiresAt,
      );
}
