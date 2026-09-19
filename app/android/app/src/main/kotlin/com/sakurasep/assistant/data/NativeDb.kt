package com.sakurasep.assistant.data

import android.content.Context
import androidx.room.Dao
import androidx.room.Database
import androidx.room.Entity
import androidx.room.Insert
import androidx.room.OnConflictStrategy
import androidx.room.PrimaryKey
import androidx.room.Query
import androidx.room.Room
import androidx.room.RoomDatabase

/**
 * 原生侧小型持久化（bridge.md §4）：assistant_native.db。
 * 与 Dart 侧 assistant_cache.db 互不干扰，仅进程内使用。
 */

@Entity(tableName = "config")
data class ConfigEntity(
    @PrimaryKey val id: Int = 1,
    val hubUrl: String = "",
    val token: String = "",
    /** api-v1 login 返回的一次性轮换 refreshToken（configure 可选透传；401 自愈用）。 */
    val refreshToken: String = "",
    val reportIntervalSeconds: Int = 30,
    val dndEnabled: Boolean = false,
    val dndStart: String = "23:00",
    val dndEnd: String = "08:00",
    val dndTimezone: String = "Asia/Shanghai",
    val lastChangeSeq: Long = 0,
    /** 用户曾主动启动服务（BootReceiver 据此决定是否拉起）。 */
    val serviceRequested: Boolean = false,
    /** 通知点击待投递路由（getLaunchPayload / consumePendingRoute 取一次即清除）。 */
    val pendingRoute: String? = null,
    val pendingRouteId: String? = null,
    val debugLogging: Boolean = false,
)

@Entity(tableName = "notified")
data class NotifiedEntity(
    @PrimaryKey val faultId: String,
    val incident: Int,
    val lastNotifiedAt: Long,
    val muted: Boolean,
)

@Entity(tableName = "held")
data class HeldEntity(
    @PrimaryKey(autoGenerate = true) val id: Long = 0,
    val sourceId: String,
    val sourceName: String,
    val title: String,
    val kind: String,
    val createdAt: Long,
)

data class HeldSourceCount(
    val sourceName: String,
    val cnt: Int,
)

@Dao
interface ConfigDao {
    @Query("SELECT * FROM config WHERE id = 1")
    suspend fun get(): ConfigEntity?

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    suspend fun upsert(config: ConfigEntity)
}

@Dao
interface NotifiedDao {
    @Query("SELECT * FROM notified WHERE faultId = :faultId")
    suspend fun get(faultId: String): NotifiedEntity?

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    suspend fun upsert(entity: NotifiedEntity)

    @Query("UPDATE notified SET muted = :muted, incident = :incident WHERE faultId = :faultId")
    suspend fun updateMute(faultId: String, muted: Boolean, incident: Int)
}

@Dao
interface HeldDao {
    @Insert(onConflict = OnConflictStrategy.REPLACE)
    suspend fun insert(entity: HeldEntity)

    @Query("SELECT COUNT(*) FROM held")
    suspend fun count(): Int

    @Query("SELECT * FROM held ORDER BY createdAt DESC, id DESC LIMIT 1")
    suspend fun latest(): HeldEntity?

    @Query("SELECT sourceName, COUNT(*) AS cnt FROM held GROUP BY sourceName ORDER BY cnt DESC")
    suspend fun perSourceCounts(): List<HeldSourceCount>

    @Query("DELETE FROM held")
    suspend fun clear()
}

@Database(
    entities = [ConfigEntity::class, NotifiedEntity::class, HeldEntity::class],
    // MVP 未发布：schema 变更直接破坏性重建（config 由下次 configure 重写）。
    version = 2,
    exportSchema = false,
)
abstract class NativeDb : RoomDatabase() {
    abstract fun configDao(): ConfigDao
    abstract fun notifiedDao(): NotifiedDao
    abstract fun heldDao(): HeldDao

    companion object {
        @Volatile
        private var instance: NativeDb? = null

        fun get(context: Context): NativeDb =
            instance ?: synchronized(this) {
                instance ?: Room.databaseBuilder(
                    context.applicationContext,
                    NativeDb::class.java,
                    "assistant_native.db",
                ).fallbackToDestructiveMigration().build().also { instance = it }
            }
    }
}
