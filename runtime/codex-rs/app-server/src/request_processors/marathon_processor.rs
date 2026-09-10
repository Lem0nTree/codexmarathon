use crate::error_code::{internal_error, invalid_params};
use crate::marathon_service::{MarathonService, MarathonServiceError};
use codex_app_server_protocol::{
    ClientResponsePayload, JSONRPCErrorError, MarathonAutoResetSetParams, MarathonEnabledSetParams,
    MarathonImportParams, MarathonSwitchParams,
};
use std::sync::Arc;

#[derive(Clone)]
pub(crate) struct MarathonRequestProcessor {
    service: Arc<MarathonService>,
}

impl MarathonRequestProcessor {
    pub(crate) fn new(service: Arc<MarathonService>) -> Self {
        Self { service }
    }

    pub(crate) fn status(&self) -> Result<Option<ClientResponsePayload>, JSONRPCErrorError> {
        self.service
            .status()
            .map(|response| Some(response.into()))
            .map_err(map_service_error)
    }

    pub(crate) async fn enabled_set(
        &self,
        params: MarathonEnabledSetParams,
    ) -> Result<Option<ClientResponsePayload>, JSONRPCErrorError> {
        self.service
            .set_enabled(params.enabled)
            .await
            .map(|response| Some(response.into()))
            .map_err(map_service_error)
    }

    pub(crate) async fn auto_reset_set(
        &self,
        params: MarathonAutoResetSetParams,
    ) -> Result<Option<ClientResponsePayload>, JSONRPCErrorError> {
        self.service
            .set_auto_reset_enabled(params.enabled)
            .await
            .map(|response| Some(response.into()))
            .map_err(map_service_error)
    }

    pub(crate) async fn switch(
        &self,
        params: MarathonSwitchParams,
    ) -> Result<Option<ClientResponsePayload>, JSONRPCErrorError> {
        self.service
            .switch(&params.target)
            .await
            .map(|response| Some(response.into()))
            .map_err(map_service_error)
    }

    pub(crate) async fn import(
        &self,
        params: MarathonImportParams,
    ) -> Result<Option<ClientResponsePayload>, JSONRPCErrorError> {
        self.service
            .import_current(&params.alias)
            .await
            .map(|response| Some(response.into()))
            .map_err(map_service_error)
    }
}

fn map_service_error(error: MarathonServiceError) -> JSONRPCErrorError {
    match error {
        MarathonServiceError::AccountNotFound
        | MarathonServiceError::AmbiguousAlias
        | MarathonServiceError::AliasConflict
        | MarathonServiceError::InvalidAlias => invalid_params(error.to_string()),
        _ => internal_error(error.to_string()),
    }
}
