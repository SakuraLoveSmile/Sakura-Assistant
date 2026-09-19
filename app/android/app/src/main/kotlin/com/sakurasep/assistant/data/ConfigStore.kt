package com.sakurasep.assistant.data

import android.content.Context

/** config 单行表的读写封装；所有字段经 load()→copy→upsert 原子更新。 */
class ConfigStore(context: Context) {
    private val dao = NativeDb.get(context).configDao()

    suspend fun load(): ConfigEntity = dao.get() ?: ConfigEntity().also { dao.upsert(it) }

    suspend fun save(config: ConfigEntity) = dao.upsert(config)

    suspend fun update(transform: (ConfigEntity) -> ConfigEntity): ConfigEntity {
        val next = transform(load())
        dao.upsert(next)
        return next
    }

    suspend fun setPendingRoute(route: String?, id: String?) {
        update { it.copy(pendingRoute = route, pendingRouteId = id) }
    }

    /** 取一次即清除（getLaunchPayload / consumePendingRoute 语义）。 */
    suspend fun consumePendingRoute(): Pair<String?, String?> {
        val c = load()
        if (c.pendingRoute != null) {
            save(c.copy(pendingRoute = null, pendingRouteId = null))
        }
        return c.pendingRoute to c.pendingRouteId
    }
}
