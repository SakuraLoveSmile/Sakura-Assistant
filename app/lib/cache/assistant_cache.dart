import 'dart:convert';

import 'package:sqflite/sqflite.dart';

import '../models/fault.dart';
import '../models/json.dart';
import '../models/message.dart';
import '../models/rules.dart';
import '../models/settings.dart';
import '../models/source.dart';
import '../models/sync.dart';

/// Dart 侧历史缓存库 `assistant_cache.db`（与原生 `assistant_native.db` 互不干扰）。
///
/// 存 sync 下来的 messages / faults / sources / rules / settings 快照 + sync
/// cursor，历史页离线可读；tombstone 增量应用。
class AssistantCache {
  AssistantCache._(this._db);

  final Database _db;

  static const dbName = 'assistant_cache.db';
  static const _version = 1;

  /// 打开（或创建）缓存库。测试可注入 `databaseFactoryFfi`。
  static Future<AssistantCache> open({
    DatabaseFactory? factory,
    String? path,
  }) async {
    final f = factory ?? databaseFactory;
    final dbPath = path ?? '${await getDatabasesPath()}/$dbName';
    final db = await f.openDatabase(
      dbPath,
      options: OpenDatabaseOptions(
        version: _version,
        onCreate: _createSchema,
      ),
    );
    return AssistantCache._(db);
  }

  /// 纯内存实例（测试用，需传 ffi factory）。
  static Future<AssistantCache> inMemory(DatabaseFactory factory) async {
    final db = await factory.openDatabase(
      inMemoryDatabasePath,
      options: OpenDatabaseOptions(version: _version, onCreate: _createSchema),
    );
    return AssistantCache._(db);
  }

  static Future<void> _createSchema(Database db, int version) async {
    await db.execute('''
      CREATE TABLE sync_state(
        id INTEGER PRIMARY KEY CHECK(id = 1),
        cursor INTEGER NOT NULL DEFAULT 0
      )
    ''');
    await db.execute('''
      CREATE TABLE messages(
        id TEXT PRIMARY KEY,
        changeSeq INTEGER NOT NULL DEFAULT 0,
        sourceId TEXT NOT NULL DEFAULT '',
        sourceName TEXT NOT NULL DEFAULT '',
        kind TEXT NOT NULL DEFAULT '',
        severity TEXT NOT NULL DEFAULT 'info',
        title TEXT NOT NULL DEFAULT '',
        body TEXT,
        occurredAt TEXT,
        receivedAt TEXT,
        readAt TEXT,
        faultId TEXT,
        incident INTEGER,
        refJson TEXT,
        attachmentsJson TEXT,
        json TEXT
      )
    ''');
    await db.execute(
        'CREATE INDEX idx_messages_received ON messages(receivedAt DESC)');
    await db.execute('CREATE INDEX idx_messages_fault ON messages(faultId)');
    await db.execute('''
      CREATE TABLE faults(
        id TEXT PRIMARY KEY,
        changeSeq INTEGER NOT NULL DEFAULT 0,
        sourceId TEXT NOT NULL DEFAULT '',
        sourceName TEXT NOT NULL DEFAULT '',
        faultKey TEXT NOT NULL DEFAULT '',
        severity TEXT NOT NULL DEFAULT 'warning',
        title TEXT NOT NULL DEFAULT '',
        summary TEXT,
        state TEXT NOT NULL DEFAULT 'open',
        incident INTEGER NOT NULL DEFAULT 1,
        openedAt TEXT,
        lastEventAt TEXT,
        resolvedAt TEXT,
        resolvedBy TEXT,
        eventCount INTEGER NOT NULL DEFAULT 0,
        readAt TEXT,
        mutedAt TEXT,
        mutedUntil TEXT,
        json TEXT
      )
    ''');
    await db.execute('CREATE INDEX idx_faults_state ON faults(state)');
    await db.execute('''
      CREATE TABLE sources(
        id TEXT PRIMARY KEY,
        changeSeq INTEGER NOT NULL DEFAULT 0,
        name TEXT NOT NULL DEFAULT '',
        kind TEXT NOT NULL DEFAULT '',
        status TEXT NOT NULL DEFAULT '',
        json TEXT NOT NULL
      )
    ''');
    await db.execute('''
      CREATE TABLE rules_meta(
        id INTEGER PRIMARY KEY CHECK(id = 1),
        version INTEGER NOT NULL DEFAULT 0,
        json TEXT NOT NULL
      )
    ''');
    await db.execute('''
      CREATE TABLE settings_meta(
        id INTEGER PRIMARY KEY CHECK(id = 1),
        version INTEGER NOT NULL DEFAULT 0,
        json TEXT NOT NULL
      )
    ''');
  }

  // ---------------- sync 应用 ----------------

  /// 当前 sync cursor（上次应用的 changeSeq）。
  Future<int> get cursor async {
    final rows = await _db.query('sync_state', where: 'id = 1');
    if (rows.isEmpty) return 0;
    return asInt(rows.first['cursor']);
  }

  /// 应用一页 sync：按 changeSeq 升序应用，然后推进 cursor。
  /// INSERT OR REPLACE 全量对象 → 天然幂等，重放同页结果一致。
  Future<void> applySyncPage(SyncPage page) async {
    final sorted = [...page.changes]
      ..sort((a, b) => a.changeSeq.compareTo(b.changeSeq));
    await _db.transaction((txn) async {
      await _applyChanges(txn, sorted);
      await txn.insert(
        'sync_state',
        {'id': 1, 'cursor': page.cursor},
        conflictAlgorithm: ConflictAlgorithm.replace,
      );
    });
  }

  /// 仅应用变更不推进 cursor（测试 / 局部刷新用）。
  Future<void> applyChanges(List<SyncChange> changes) async {
    final sorted = [...changes]
      ..sort((a, b) => a.changeSeq.compareTo(b.changeSeq));
    await _db.transaction((txn) => _applyChanges(txn, sorted));
  }

  Future<void> _applyChanges(
      DatabaseExecutor txn, List<SyncChange> changes) async {
    for (final change in changes) {
      switch (change.type) {
        case 'message':
          await _upsertMessage(txn, Message.fromJson(change.data), change.data);
        case 'fault':
          await _upsertFault(txn, Fault.fromJson(change.data), change.data);
        case 'source':
          await _upsertSource(txn, change.data);
        case 'rules':
          await txn.insert(
            'rules_meta',
            {
              'id': 1,
              'version': asInt(change.data['version']),
              'json': jsonEncode(change.data),
            },
            conflictAlgorithm: ConflictAlgorithm.replace,
          );
        case 'settings':
          await txn.insert(
            'settings_meta',
            {
              'id': 1,
              'version': asInt(change.data['version']),
              'json': jsonEncode(change.data),
            },
            conflictAlgorithm: ConflictAlgorithm.replace,
          );
        case 'tombstone':
          await _applyTombstone(txn, change.data);
        default:
          // 未知变更类型（契约允许新增）：忽略不崩。
          break;
      }
    }
  }

  Future<void> _applyTombstone(
      DatabaseExecutor txn, Map<String, dynamic> data) async {
    final type = asString(data['type']);
    final id = asString(data['id']);
    if (id.isEmpty) return;
    switch (type) {
      case 'message':
        await txn.delete('messages', where: 'id = ?', whereArgs: [id]);
      case 'fault':
        await txn.delete('faults', where: 'id = ?', whereArgs: [id]);
      case 'source':
        await txn.delete('sources', where: 'id = ?', whereArgs: [id]);
    }
  }

  Future<void> _upsertMessage(
      DatabaseExecutor txn, Message m, Map<String, dynamic> raw) async {
    await txn.insert(
      'messages',
      {
        'id': m.id,
        'changeSeq': m.changeSeq,
        'sourceId': m.sourceId,
        'sourceName': m.sourceName,
        'kind': m.kind,
        'severity': m.severity,
        'title': m.title,
        'body': m.body,
        'occurredAt': m.occurredAt.toUtc().toIso8601String(),
        'receivedAt': m.receivedAt.toUtc().toIso8601String(),
        'readAt': m.readAt?.toUtc().toIso8601String(),
        'faultId': m.faultId,
        'incident': m.incident,
        'refJson': m.ref == null ? null : jsonEncode(m.ref),
        'attachmentsJson': jsonEncode(m.attachments.map((a) => a.toJson()).toList()),
        'json': jsonEncode(raw),
      },
      conflictAlgorithm: ConflictAlgorithm.replace,
    );
  }

  Future<void> _upsertFault(
      DatabaseExecutor txn, Fault f, Map<String, dynamic> raw) async {
    await txn.insert(
      'faults',
      {
        'id': f.id,
        'changeSeq': f.changeSeq,
        'sourceId': f.sourceId,
        'sourceName': f.sourceName,
        'faultKey': f.faultKey,
        'severity': f.severity,
        'title': f.title,
        'summary': f.summary,
        'state': f.state,
        'incident': f.incident,
        'openedAt': f.openedAt.toUtc().toIso8601String(),
        'lastEventAt': f.lastEventAt.toUtc().toIso8601String(),
        'resolvedAt': f.resolvedAt?.toUtc().toIso8601String(),
        'resolvedBy': f.resolvedBy?.toString(),
        'eventCount': f.eventCount,
        'readAt': f.readAt?.toUtc().toIso8601String(),
        'mutedAt': f.mutedAt?.toUtc().toIso8601String(),
        'mutedUntil': f.mutedUntil?.toUtc().toIso8601String(),
        'json': jsonEncode(raw),
      },
      conflictAlgorithm: ConflictAlgorithm.replace,
    );
  }

  Future<void> _upsertSource(
      DatabaseExecutor txn, Map<String, dynamic> data) async {
    final source = Source.fromJson(data);
    if (source.id.isEmpty) return;
    await txn.insert(
      'sources',
      {
        'id': source.id,
        'changeSeq': asInt(data['changeSeq']),
        'name': source.name,
        'kind': source.kind,
        'status': source.status,
        'json': jsonEncode(data),
      },
      conflictAlgorithm: ConflictAlgorithm.replace,
    );
  }

  /// 直接写单条消息（REST 写操作返回的最新对象即时落缓存，等 sync 收敛）。
  Future<void> upsertMessage(Message m) =>
      _db.transaction((txn) => _upsertMessage(txn, m, m.toJson()));

  /// 直接写单条故障。
  Future<void> upsertFault(Fault f) =>
      _db.transaction((txn) => _upsertFault(txn, f, f.toJson()));

  /// 直接写单个来源（管理操作后本地同步）。
  Future<void> upsertSource(Source s) =>
      _db.transaction((txn) => _upsertSource(txn, s.toJson()));

  /// 删除来源。
  Future<void> deleteSource(String id) =>
      _db.delete('sources', where: 'id = ?', whereArgs: [id]);

  /// 直接写 rules / settings 快照。
  Future<void> saveRules(Rules rules) => _db.insert(
        'rules_meta',
        {'id': 1, 'version': rules.version, 'json': jsonEncode({
          'version': rules.version,
          'heartbeatSeconds': rules.heartbeatSeconds,
          'rules': rules.rules.map((r) => r.toJson()).toList(),
        })},
        conflictAlgorithm: ConflictAlgorithm.replace,
      );

  Future<void> saveSettings(Settings settings) => _db.insert(
        'settings_meta',
        {'id': 1, 'version': settings.version, 'json': jsonEncode({
          'version': settings.version,
          'dnd': settings.dnd.toJson(),
          'reportIntervalSeconds': settings.reportIntervalSeconds,
        })},
        conflictAlgorithm: ConflictAlgorithm.replace,
      );

  // ---------------- 读取 ----------------

  Message _messageFromRow(Map<String, Object?> row) {
    final refJson = row['refJson'] as String?;
    final attJson = row['attachmentsJson'] as String?;
    return Message(
      id: asString(row['id']),
      changeSeq: asInt(row['changeSeq']),
      sourceId: asString(row['sourceId']),
      sourceName: asString(row['sourceName']),
      kind: asString(row['kind']),
      severity: asString(row['severity'], 'info'),
      title: asString(row['title']),
      body: row['body'] as String?,
      occurredAt: asDate(row['occurredAt']),
      receivedAt: asDate(row['receivedAt']),
      readAt: asDateOrNull(row['readAt']),
      faultId: row['faultId'] as String?,
      incident: asIntOrNull(row['incident']),
      ref: refJson == null ? null : asMap(jsonDecode(refJson)),
      attachments: attJson == null
          ? const []
          : asMapList(jsonDecode(attJson)).map(Attachment.fromJson).toList(),
    );
  }

  Fault _faultFromRow(Map<String, Object?> row) => Fault(
        id: asString(row['id']),
        changeSeq: asInt(row['changeSeq']),
        sourceId: asString(row['sourceId']),
        sourceName: asString(row['sourceName']),
        faultKey: asString(row['faultKey']),
        severity: asString(row['severity'], 'warning'),
        title: asString(row['title']),
        summary: row['summary'] as String?,
        state: asString(row['state'], 'open'),
        incident: asInt(row['incident'], 1),
        openedAt: asDate(row['openedAt']),
        lastEventAt: asDate(row['lastEventAt']),
        resolvedAt: asDateOrNull(row['resolvedAt']),
        resolvedBy: row['resolvedBy'],
        eventCount: asInt(row['eventCount']),
        readAt: asDateOrNull(row['readAt']),
        mutedAt: asDateOrNull(row['mutedAt']),
        mutedUntil: asDateOrNull(row['mutedUntil']),
      );

  /// 本地历史分页。`filter` 语义对齐契约（feedback=feedback 来源独立消息，
  /// faults=属于故障的消息）；offset 分页。
  Future<List<Message>> messages({
    String filter = 'all',
    String? sourceId,
    int limit = 50,
    int offset = 0,
  }) async {
    final where = <String>[];
    final args = <Object?>[];
    switch (filter) {
      case 'unread':
        where.add('readAt IS NULL');
      case 'feedback':
        where.add(
            "faultId IS NULL AND sourceId IN (SELECT id FROM sources WHERE kind = 'feedback')");
      case 'faults':
        where.add('faultId IS NOT NULL');
      default:
        break;
    }
    if (sourceId != null && sourceId.isNotEmpty) {
      where.add('sourceId = ?');
      args.add(sourceId);
    }
    final rows = await _db.query(
      'messages',
      where: where.isEmpty ? null : where.join(' AND '),
      whereArgs: args.isEmpty ? null : args,
      orderBy: 'receivedAt DESC',
      limit: limit,
      offset: offset,
    );
    return rows.map(_messageFromRow).toList();
  }

  Future<Message?> messageById(String id) async {
    final rows = await _db.query('messages', where: 'id = ?', whereArgs: [id]);
    return rows.isEmpty ? null : _messageFromRow(rows.first);
  }

  /// 某故障的全部消息（轮次时间线数据源）。
  Future<List<Message>> messagesForFault(String faultId) async {
    final rows = await _db.query(
      'messages',
      where: 'faultId = ?',
      whereArgs: [faultId],
      orderBy: 'occurredAt ASC',
    );
    return rows.map(_messageFromRow).toList();
  }

  Future<Fault?> faultById(String id) async {
    final rows = await _db.query('faults', where: 'id = ?', whereArgs: [id]);
    return rows.isEmpty ? null : _faultFromRow(rows.first);
  }

  Future<List<Fault>> faults({String state = 'all'}) async {
    final rows = await _db.query(
      'faults',
      where: state == 'all' ? null : 'state = ?',
      whereArgs: state == 'all' ? null : [state],
      orderBy: 'lastEventAt DESC',
    );
    return rows.map(_faultFromRow).toList();
  }

  Future<List<Fault>> openFaults() => faults(state: 'open');

  Future<int> unreadMessageCount() async {
    final rows = await _db
        .rawQuery('SELECT COUNT(*) AS c FROM messages WHERE readAt IS NULL');
    return asInt(rows.first['c']);
  }

  Future<List<Source>> sources() async {
    final rows = await _db.query('sources', orderBy: 'name ASC');
    return rows
        .map((r) => Source.fromJson(asMap(jsonDecode(asString(r['json'])))))
        .toList();
  }

  Future<Source?> sourceById(String id) async {
    final rows = await _db.query('sources', where: 'id = ?', whereArgs: [id]);
    if (rows.isEmpty) return null;
    return Source.fromJson(asMap(jsonDecode(asString(rows.first['json']))));
  }

  Future<Rules?> rules() async {
    final rows = await _db.query('rules_meta', where: 'id = 1');
    if (rows.isEmpty) return null;
    return Rules.fromJson(asMap(jsonDecode(asString(rows.first['json']))));
  }

  Future<Settings?> settings() async {
    final rows = await _db.query('settings_meta', where: 'id = 1');
    if (rows.isEmpty) return null;
    return Settings.fromJson(asMap(jsonDecode(asString(rows.first['json']))));
  }

  /// 读变更推进前的最大 changeSeq（调试用）。
  Future<int> maxMessageChangeSeq() async {
    final rows =
        await _db.rawQuery('SELECT MAX(changeSeq) AS c FROM messages');
    return asInt(rows.first['c']);
  }

  Future<void> close() => _db.close();
}
