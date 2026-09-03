package main

import (
	"errors"
	"testing"
)

func TestAddProjectMemberCannotDemoteSoleOwner(t *testing.T) {
	store := newTestStore(t)
	owner, err := store.CreateUser("owner@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(owner.ID, "Shared", "", "")
	if err != nil {
		t.Fatal(err)
	}

	err = store.AddProjectMember(project.ID, owner.ID, ProjectEditor, owner.ID)
	if !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demote sole owner error=%v, want ErrLastOwner", err)
	}
	role, err := store.GetProjectRole(project.ID, owner.ID)
	if err != nil || role != ProjectOwner {
		t.Fatalf("sole owner role=%q err=%v, want owner", role, err)
	}
}

func TestAcceptInviteCannotDemoteSoleOwner(t *testing.T) {
	store := newTestStore(t)
	owner, err := store.CreateUser("owner@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(owner.ID, "Shared", "", "")
	if err != nil {
		t.Fatal(err)
	}
	invite, err := store.CreateInvite(project.ID, owner.Email, ProjectViewer, owner.ID, 0)
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.AcceptInvite(invite.ID, owner.ID)
	if !errors.Is(err, ErrLastOwner) {
		t.Fatalf("accept demoting invite error=%v, want ErrLastOwner", err)
	}
	role, err := store.GetProjectRole(project.ID, owner.ID)
	if err != nil || role != ProjectOwner {
		t.Fatalf("sole owner role=%q err=%v, want owner", role, err)
	}
}

func TestDeleteUserRequiresAndTransfersSharedProjectOwnership(t *testing.T) {
	store := newTestStore(t)
	alice, err := store.CreateUser("alice@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.CreateUser("bob@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(alice.ID, "Shared", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddProjectMember(project.ID, bob.ID, ProjectEditor, alice.ID); err != nil {
		t.Fatal(err)
	}
	sharedAgent, err := store.CreateAgent(alice.ID, "shared-agent", "", "autonomous", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}

	err = store.ValidateUserDeletion(alice.ID)
	if !errors.Is(err, ErrUserOwnsSharedProject) {
		t.Fatalf("delete preflight error=%v, want ErrUserOwnsSharedProject", err)
	}
	err = store.DeleteUser(alice.ID)
	if !errors.Is(err, ErrUserOwnsSharedProject) {
		t.Fatalf("transactional delete guard error=%v, want ErrUserOwnsSharedProject", err)
	}
	if _, err := store.GetUserByID(alice.ID); err != nil {
		t.Fatalf("blocked deletion removed user: %v", err)
	}

	if err := store.AddProjectMember(project.ID, bob.ID, ProjectOwner, alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUser(alice.ID); err != nil {
		t.Fatalf("delete after ownership transfer: %v", err)
	}
	got, err := store.GetProjectAny(project.ID)
	if err != nil {
		t.Fatalf("shared project was deleted: %v", err)
	}
	if got.UserID != bob.ID {
		t.Fatalf("legacy project owner=%d, want %d", got.UserID, bob.ID)
	}
	if role, err := store.GetProjectRole(project.ID, bob.ID); err != nil || role != ProjectOwner {
		t.Fatalf("new owner role=%q err=%v", role, err)
	}
	gotAgent, err := store.GetAgentByID(sharedAgent.ID)
	if err != nil {
		t.Fatalf("shared project agent was deleted: %v", err)
	}
	if gotAgent.UserID != bob.ID {
		t.Fatalf("shared project agent owner=%d, want %d", gotAgent.UserID, bob.ID)
	}
}

func TestDeleteAgentAllowsProjectEditorButNotViewer(t *testing.T) {
	store := newTestStore(t)
	owner, _ := store.CreateUser("owner@example.com", "hash")
	editor, _ := store.CreateUser("editor@example.com", "hash")
	viewer, _ := store.CreateUser("viewer@example.com", "hash")
	project, err := store.CreateProject(owner.ID, "Shared", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddProjectMember(project.ID, editor.ID, ProjectEditor, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AddProjectMember(project.ID, viewer.ID, ProjectViewer, owner.ID); err != nil {
		t.Fatal(err)
	}

	viewerTarget, err := store.CreateAgent(owner.ID, "viewer-target", "", "autonomous", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAgent(viewer.ID, viewerTarget.ID); err == nil {
		t.Fatal("viewer deleted a project agent")
	}
	if _, err := store.GetAgentByID(viewerTarget.ID); err != nil {
		t.Fatalf("viewer attempt removed project agent: %v", err)
	}

	editorTarget, err := store.CreateAgent(owner.ID, "editor-target", "", "autonomous", "{}", project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAgent(editor.ID, editorTarget.ID); err != nil {
		t.Fatalf("editor could not delete project agent: %v", err)
	}
	if _, err := store.GetAgentByID(editorTarget.ID); err == nil {
		t.Fatal("editor-authorized project agent still exists")
	}
}
